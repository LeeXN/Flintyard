package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeBootstrapPublishHelper(t *testing.T) {
	if os.Getenv("FY_BOOTSTRAP_HELPER") != "1" {
		return
	}
	args := os.Args
	n := len(args)
	if n < 4 {
		t.Fatal("missing bootstrap helper parameters")
	}
	path, root, id := args[n-3], args[n-2], args[n-1]
	err := ownedDirContents(path, Owner{RootID: root, EnvironmentID: id}, map[string]any{"state.json": map[string]bool{"test_only": true}}, func(stage string) error {
		fmt.Println(stage)
		for {
			time.Sleep(time.Second)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeBootstrapSIGKILLThenPublicGCArchivesMarkedStage(t *testing.T) {
	if os.Getenv("FY_BOOTSTRAP_INTEGRATION") != "1" {
		t.Skip("requires tests/native-bootstrap.sh; no real bootstrap crash checked")
	}
	if os.Geteuid() != 0 || os.Getenv("FY_BOOTSTRAP_CLI") == "" {
		t.Fatal("native bootstrap prerequisites missing")
	}
	root, err := OpenRoot(t.Context(), filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	id := UUID()
	destination := filepath.Join(root.Path, "envs", id)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestNativeBootstrapPublishHelper$", "--", destination, root.ID, id)
	cmd.Env = append(os.Environ(), "FY_BOOTSTRAP_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- strings.TrimSpace(line) }()
	var stage string
	select {
	case stage = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap checkpoint not reached")
	}
	marker, err := ReadBootstrap(stage)
	if err != nil || marker.Destination != destination {
		t.Fatal(marker, err)
	}
	cli := os.Getenv("FY_BOOTSTRAP_CLI")
	run := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(cli, args...)
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatal(args, err, string(data))
		}
		return data
	}
	var live struct {
		OK   bool `json:"ok"`
		Data struct {
			Plan struct {
				Bootstraps []any `json:"bootstraps"`
			} `json:"plan"`
			Excluded []struct {
				Reason string `json:"reason"`
			} `json:"bootstraps_excluded"`
		} `json:"data"`
	}
	if err = json.Unmarshal(run("gc", "--state-dir", root.Path, "--json"), &live); err != nil || !live.OK || len(live.Data.Plan.Bootstraps) != 0 || len(live.Data.Excluded) != 1 || !strings.Contains(live.Data.Excluded[0].Reason, "alive") {
		t.Fatal(live, err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("publisher not killed")
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	run("gc", "--dry-run", "--plan", plan, "--state-dir", root.Path, "--json")
	var applied struct {
		OK   bool `json:"ok"`
		Data struct {
			Applied bool     `json:"applied"`
			Paths   []string `json:"bootstraps_cleaned"`
		} `json:"data"`
	}
	if err = json.Unmarshal(run("gc", "--apply", "--plan", plan, "--state-dir", root.Path, "--json"), &applied); err != nil || !applied.OK || !applied.Data.Applied || len(applied.Data.Paths) != 1 {
		t.Fatal(applied, err)
	}
	if _, err = os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("staging entry retained", err)
	}
	if _, err = os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("unpublished environment adopted", err)
	}
	archive := filepath.Join(root.Path, "diagnostics", "bootstrap", marker.Token)
	got, err := ReadBootstrap(archive)
	if err != nil || got.Token != marker.Token || got.Owner != marker.Owner {
		t.Fatal(got, err)
	}
	if _, err = os.Stat(filepath.Join(archive, "gc-receipt.json")); err != nil {
		t.Fatal(err)
	}
}
