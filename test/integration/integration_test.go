//go:build integration

// Package integration drives the real talos binary over stdio against a real
// foreground OpenCode server with isolated XDG directories, so nothing touches the
// user's service or database. It uses a free model, and therefore only scratch
// repositories, because free providers may keep prompts.
//
// Run with: go test -tags integration ./test/integration
package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const password = "integration"

var (
	serverURL string
	talosBin  string
	model     = "opencode/deepseek-v4.1-flash-free"
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if _, err := exec.LookPath("opencode"); err != nil {
		fmt.Println("skipping: opencode is not installed")
		return 0
	}
	if override := os.Getenv("TALOS_TEST_MODEL"); override != "" {
		model = override
	}
	dir, err := os.MkdirTemp("", "talos-integration-")
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer os.RemoveAll(dir)

	talosBin = filepath.Join(dir, "talos")
	build := exec.Command("go", "build", "-o", talosBin, "go.xchunk.org/talos/cmd/talos")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("build: %v: %s\n", err, out)
		return 1
	}

	port, err := freePort()
	if err != nil {
		fmt.Println(err)
		return 1
	}
	serve := exec.Command("opencode", "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	serve.Env = append(isolatedEnv(dir), "OPENCODE_SERVER_PASSWORD="+password)
	if err := serve.Start(); err != nil {
		fmt.Println(err)
		return 1
	}
	defer func() {
		serve.Process.Signal(syscall.SIGTERM)
		serve.Wait()
	}()
	serverURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; ; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			conn.Close()
			break
		}
		if i > 100 {
			fmt.Println("opencode serve did not come up")
			return 1
		}
		time.Sleep(200 * time.Millisecond)
	}
	return m.Run()
}

func isolatedEnv(dir string) []string {
	env := os.Environ()
	for _, name := range []string{"config", "data", "state", "cache"} {
		env = append(env, "XDG_"+strings.ToUpper(name)+"_HOME="+filepath.Join(dir, name))
	}
	return env
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// client speaks MCP to one talos process. Calls run one at a time.
type client struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
	next  int
	done  sync.Once
}

// launch starts talos and completes the MCP handshake.
func launch(t *testing.T, repo, state, url string, env ...string) *client {
	t.Helper()
	cmd := exec.Command(talosBin)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"TALOS_OPENCODE_URL="+url,
		"TALOS_OPENCODE_PASSWORD="+password,
		"TALOS_STATE_DIR="+state,
		"TALOS_ALLOWED_ROOTS="+repo,
		"TALOS_DEFAULT_MODEL="+model,
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
	t.Cleanup(c.stop)
	c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "integration", "version": "1"},
	})
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return c
}

// stop ends talos the way a closing orchestrator does.
func (c *client) stop() {
	c.done.Do(func() {
		c.stdin.Close()
		exited := make(chan struct{})
		go func() {
			c.cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			c.t.Error("talos did not exit after stdin closed")
			c.cmd.Process.Kill()
		}
	})
}

// kill ends talos with no chance to clean up.
func (c *client) kill() {
	c.done.Do(func() {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	})
}

func (c *client) send(message map[string]any) {
	c.t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) request(method string, params any) json.RawMessage {
	c.t.Helper()
	c.next++
	id := c.next
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("read response to %s: %v", method, err)
		}
		var response struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil {
			c.t.Fatalf("response %q: %v", line, err)
		}
		if response.ID == nil || *response.ID != id {
			continue
		}
		if response.Error != nil {
			c.t.Fatalf("%s: %s", method, response.Error)
		}
		return response.Result
	}
}

// tryCall invokes a tool and returns its text output and whether it is an error.
func (c *client) tryCall(tool string, args map[string]any) (string, bool) {
	c.t.Helper()
	raw := c.request("tools/call", map[string]any{"name": tool, "arguments": args})
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		c.t.Fatal(err)
	}
	var b strings.Builder
	for _, block := range result.Content {
		b.WriteString(block.Text)
	}
	return b.String(), result.IsError
}

func (c *client) call(tool string, args map[string]any, out any) {
	c.t.Helper()
	text, isError := c.tryCall(tool, args)
	if isError {
		c.t.Fatalf("%s: %s", tool, text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			c.t.Fatalf("%s: %q: %v", tool, text, err)
		}
	}
}

type pending struct {
	RequestID string   `json:"request_id"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

type status struct {
	TaskID  string    `json:"task_id"`
	State   string    `json:"state"`
	Steps   int       `json:"steps"`
	Error   string    `json:"error"`
	Pending []pending `json:"pending_permissions"`
}

type started struct {
	TaskID   string `json:"task_id"`
	State    string `json:"state"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
}

type result struct {
	status
	Message       string `json:"message"`
	DiffStat      string `json:"diff_stat"`
	DeniedActions int    `json:"denied_actions"`
}

func (c *client) start(repo, prompt string, extra map[string]any) started {
	c.t.Helper()
	args := map[string]any{"directory": repo, "prompt": prompt}
	for k, v := range extra {
		args[k] = v
	}
	var out started
	c.call("start_task", args, &out)
	return out
}

// waitFor blocks until the task reaches a wanted state, failing on any other state
// it would not leave by itself.
func (c *client) waitFor(id string, want ...string) status {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var out struct {
			Tasks []status `json:"tasks"`
		}
		c.call("wait_task", map[string]any{"task_ids": []string{id}, "timeout_seconds": 20}, &out)
		got := out.Tasks[0]
		// An exhausted quota says nothing about Talos, and failing here would read
		// as a defect. It is a skip, so a run with skips has not proven the scenario.
		if strings.Contains(got.Error, "provider.quota") {
			c.t.Skipf("model quota exhausted: %s", got.Error)
		}
		for _, state := range want {
			if got.State == state {
				return got
			}
		}
		if got.State != "queued" && got.State != "running" {
			c.t.Fatalf("task reached %s (%s), want %v", got.State, got.Error, want)
		}
	}
	c.t.Fatalf("task %s did not reach %v in time", id, want)
	return status{}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "README.md"), "scratch\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const writePrompt = "Use the write tool to create a file named %s containing exactly the word hello. Then reply with the single word done. Do nothing else."

func TestReviewAndMerge(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)

	var server struct {
		Reachable bool   `json:"reachable"`
		Version   string `json:"version"`
	}
	c.call("server_status", map[string]any{}, &server)
	if !server.Reachable || !strings.HasPrefix(server.Version, "2.") {
		t.Fatalf("server = %+v", server)
	}

	task := c.start(repo, fmt.Sprintf(writePrompt, "hello.txt"), nil)
	c.waitFor(task.TaskID, "succeeded")

	var res result
	c.call("get_result", map[string]any{"task_id": task.TaskID}, &res)
	if !strings.Contains(res.DiffStat, "hello.txt") || res.Message == "" {
		t.Errorf("result = %+v", res)
	}
	var diff struct {
		Text string `json:"text"`
	}
	c.call("get_diff", map[string]any{"task_id": task.TaskID}, &diff)
	if !strings.Contains(diff.Text, "+hello") {
		t.Errorf("diff = %q", diff.Text)
	}
	var transcript struct {
		Text string `json:"text"`
	}
	c.call("get_transcript", map[string]any{"task_id": task.TaskID}, &transcript)
	if !strings.Contains(transcript.Text, "tool: write") {
		t.Errorf("transcript = %q", transcript.Text)
	}

	if text, isError := c.tryCall("cleanup_task", map[string]any{"task_id": task.TaskID}); !isError {
		t.Errorf("unmerged work must be refused: %s", text)
	}
	c.call("cleanup_task", map[string]any{"task_id": task.TaskID, "merge": true, "commit_message": "feat: add hello"}, nil)
	if !exists(filepath.Join(repo, "hello.txt")) {
		t.Error("the change did not reach the source checkout")
	}
	if exists(task.Worktree) {
		t.Error("the worktree remains")
	}
	if out := git(t, repo, "branch", "--list", "talos/*"); out != "" {
		t.Errorf("branches remain: %s", out)
	}
	if out := git(t, repo, "worktree", "list"); strings.Contains(out, "\n") {
		t.Errorf("worktrees remain: %s", out)
	}
}

func TestFollowUpKeepsContext(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, "Remember the code word pineapple. Reply with the single word ok. Do not use tools.", nil)
	c.waitFor(task.TaskID, "succeeded")
	c.call("send_message", map[string]any{"task_id": task.TaskID, "text": "What was the code word? Reply with only that word."}, nil)
	time.Sleep(500 * time.Millisecond)
	c.waitFor(task.TaskID, "succeeded")
	var res result
	c.call("get_result", map[string]any{"task_id": task.TaskID}, &res)
	if !strings.Contains(strings.ToLower(res.Message), "pineapple") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestParallelTasksWithQueue(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL, "TALOS_MAX_PARALLEL=3")
	var tasks []started
	for i := range 4 {
		tasks = append(tasks, c.start(repo, fmt.Sprintf(writePrompt, fmt.Sprintf("file%d.txt", i)), nil))
	}
	var fourth status
	c.call("get_task", map[string]any{"task_id": tasks[3].TaskID}, &fourth)
	if fourth.State != "queued" {
		t.Errorf("fourth task = %s, want queued", fourth.State)
	}
	for i, task := range tasks {
		c.waitFor(task.TaskID, "succeeded")
		for j := range 4 {
			name := filepath.Join(task.Worktree, fmt.Sprintf("file%d.txt", j))
			if exists(name) != (i == j) {
				t.Errorf("task %d: %s exists = %v", i, name, exists(name))
			}
		}
	}
}

func TestDeniedShellPatternIsBlocked(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, "Run this exact command with the shell tool: git remote add evil https://example.com/x.git\nIf it is blocked, do not retry and do not work around it. Reply with the word finished.", nil)
	c.waitFor(task.TaskID, "succeeded")
	var res result
	c.call("get_result", map[string]any{"task_id": task.TaskID}, &res)
	if res.DeniedActions == 0 {
		t.Errorf("denied_actions = 0, message = %q", res.Message)
	}
	if out := git(t, repo, "remote"); out != "" {
		t.Errorf("the remote was added: %s", out)
	}
}

func TestWriteOutsideWorktreeIsBlocked(t *testing.T) {
	repo := newRepo(t)
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, fmt.Sprintf("Use the write tool to create the file at the absolute path %s containing hello. Use only the write tool, not the shell. If it is blocked, do not retry. Reply with the word finished.", outside), nil)
	c.waitFor(task.TaskID, "succeeded")
	if exists(outside) {
		t.Error("the file tool escaped the worktree")
	}
}

func TestAskRuleRootSession(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL, "TALOS_SHELL_ASK=touch *")
	task := c.start(repo, "Run this exact command with the shell tool: touch approved.txt\nThen reply with the single word done.", nil)
	got := c.waitFor(task.TaskID, "waiting_permission")
	if len(got.Pending) != 1 || got.Pending[0].Action != "shell" {
		t.Fatalf("pending = %+v", got.Pending)
	}
	c.call("respond_permission", map[string]any{"task_id": task.TaskID, "request_id": got.Pending[0].RequestID, "decision": "allow"}, nil)
	c.waitFor(task.TaskID, "succeeded")
	if !exists(filepath.Join(task.Worktree, "approved.txt")) {
		t.Error("the approved command did not run")
	}
}

func TestAskRuleChildSession(t *testing.T) {
	repo := newRepo(t)
	state := t.TempDir()
	c := launch(t, repo, state, serverURL, "TALOS_SHELL_ASK=touch *")
	task := c.start(repo, "Use the task tool to launch a general sub-agent, and tell it to run this exact shell command: touch child.txt\nDo not run the command yourself. When the sub-agent finishes, reply with the single word done.", nil)
	got := c.waitFor(task.TaskID, "waiting_permission")
	if len(got.Pending) == 0 {
		t.Fatal("no pending request")
	}
	record := readRecord(t, state, task.TaskID)
	if len(record.Children) == 0 {
		t.Fatalf("no child session recorded: the model may have skipped the task tool: %+v", record)
	}
	c.call("respond_permission", map[string]any{"task_id": task.TaskID, "request_id": got.Pending[0].RequestID, "decision": "allow"}, nil)
	c.waitFor(task.TaskID, "succeeded")
	if !exists(filepath.Join(task.Worktree, "child.txt")) {
		t.Error("the approved command did not run")
	}
}

type record struct {
	Children []string `json:"children"`
}

func readRecord(t *testing.T, state, id string) record {
	t.Helper()
	var out record
	var last error
	for range 50 {
		raw, err := os.ReadFile(filepath.Join(state, "tasks", id+".json"))
		if err == nil {
			if err = json.Unmarshal(raw, &out); err == nil && len(out.Children) > 0 {
				return out
			}
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		t.Fatal(last)
	}
	return out
}

const sleepPrompt = "Run this exact command with the shell tool and wait for it: sleep 120\nThen reply with the single word done."

// waitSteps waits until the model has begun the turn, so an interruption lands on
// real work.
func (c *client) waitSteps(id string) {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var got status
		c.call("get_task", map[string]any{"task_id": id}, &got)
		if got.Steps > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatal("the model never started")
}

func TestAbortKeepsWorktree(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, sleepPrompt, nil)
	c.waitSteps(task.TaskID)
	c.call("abort_task", map[string]any{"task_id": task.TaskID}, nil)
	c.waitFor(task.TaskID, "aborted")
	if !exists(task.Worktree) {
		t.Error("the worktree was removed")
	}
}

func TestTimeoutKeepsWorktree(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, sleepPrompt, map[string]any{"timeout_seconds": 5})
	got := c.waitFor(task.TaskID, "timed_out")
	if !strings.Contains(got.Error, "timed out") {
		t.Errorf("error = %q", got.Error)
	}
	if !exists(task.Worktree) {
		t.Error("the worktree was removed")
	}
}

func TestStepLimitFails(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, "Create ten files named a1.txt to a10.txt, one write tool call at a time. Then reply done.", map[string]any{"max_steps": 1})
	got := c.waitFor(task.TaskID, "failed")
	if !strings.Contains(got.Error, "step limit") {
		t.Errorf("error = %q", got.Error)
	}
	if !exists(task.Worktree) {
		t.Error("the worktree was removed")
	}
}

func TestBadModelFails(t *testing.T) {
	repo := newRepo(t)
	c := launch(t, repo, t.TempDir(), serverURL)
	task := c.start(repo, "Say hi.", map[string]any{"model": "opencode/no-such-model"})
	got := c.waitFor(task.TaskID, "failed")
	if got.Error == "" {
		t.Error("the failure has no error")
	}
	if !exists(task.Worktree) {
		t.Error("the worktree was removed")
	}
}

func TestKilledTalosAdoptsRunningTask(t *testing.T) {
	repo := newRepo(t)
	state := t.TempDir()
	first := launch(t, repo, state, serverURL)
	task := first.start(repo, "Run this exact command with the shell tool and wait for it: sleep 15\nThen use the write tool to create after.txt containing hello. Then reply with the single word done.", nil)
	first.waitSteps(task.TaskID)
	first.kill()

	second := launch(t, repo, state, serverURL)
	second.waitFor(task.TaskID, "succeeded")
	if !exists(filepath.Join(task.Worktree, "after.txt")) {
		t.Error("the work did not finish")
	}
}

func TestTaskFinishedWhileTalosWasDown(t *testing.T) {
	repo := newRepo(t)
	state := t.TempDir()
	first := launch(t, repo, state, serverURL)
	name := "whiledown.txt"
	task := first.start(repo, fmt.Sprintf(writePrompt, name), nil)
	first.waitSteps(task.TaskID)
	first.kill()

	path := filepath.Join(task.Worktree, name)
	deadline := time.Now().Add(3 * time.Minute)
	for !exists(path) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(10 * time.Second) // Let the turn close after the file appears.

	second := launch(t, repo, state, serverURL)
	second.waitFor(task.TaskID, "succeeded")
}

// proxy forwards TCP to the server and can sever every connection, which models a
// dropped event stream without touching the server.
type proxy struct {
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{listener: l, conns: make(map[net.Conn]struct{})}
	t.Cleanup(func() {
		l.Close()
		p.sever()
	})
	go func() {
		for {
			down, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				down.Close()
				continue
			}
			p.track(down, up)
			go pipe(up, down)
			go pipe(down, up)
		}
	}()
	return p
}

func pipe(dst, src net.Conn) {
	io.Copy(dst, src)
	dst.Close()
	src.Close()
}

func (p *proxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
}

func (p *proxy) sever() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
	clear(p.conns)
}

func TestSeveredStreamRecovers(t *testing.T) {
	repo := newRepo(t)
	p := newProxy(t, strings.TrimPrefix(serverURL, "http://"))
	c := launch(t, repo, t.TempDir(), "http://"+p.listener.Addr().String())
	task := c.start(repo, "Run this exact command with the shell tool and wait for it: sleep 8\nThen reply with the single word done.", nil)
	c.waitSteps(task.TaskID)
	p.sever()
	c.waitFor(task.TaskID, "succeeded")
}
