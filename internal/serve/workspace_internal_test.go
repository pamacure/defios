package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"defios.com/internal/cosl"
	"defios.com/internal/cosl/coslbench"
	"defios.com/internal/group"
	"defios.com/internal/key"
	"defios.com/internal/nodedir"
)

// watcherOn is a Workspace watcher over an empty replica, logging to lines.
func watcherOn(t *testing.T, lines *[]string) (*server, *cosl.Store, *workspaceWatcher) {
	t.Helper()
	store, err := cosl.Open(filepath.Join(t.TempDir(), "cosl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := &server{opts: Options{
		Paths: nodedir.Resolve(nodedir.Lookup{Dir: filepath.Join(t.TempDir(), "node")}),
		Alias: group.ImplicitAlias,
		Logf:  func(format string, args ...any) { *lines = append(*lines, fmt.Sprintf(format, args...)) },
	}}
	return s, store, s.newWorkspaceWatcher(store)
}

func workspaceNames(t *testing.T, store *cosl.Store) map[[32]byte]string {
	t.Helper()
	keys, err := key.Read(store)
	if err != nil {
		t.Fatal(err)
	}
	return key.WorkspaceNames(keys)
}

// Keys that arrive with a snapshot restore get their Workspaces on the
// watcher's next wake, as Keys minted one row at a time do.
func TestWorkspaceWatcherFollowsARestore(t *testing.T) {
	var lines []string
	s, store, w := watcherOn(t, &lines)
	w.wake()

	shape := coslbench.Shapes[0]
	if err := store.Restore(coslbench.Build(t, shape).Path); err != nil {
		t.Fatal(err)
	}
	w.wake()
	names := workspaceNames(t, store)
	if len(names) != shape.ECDSAKeys+shape.EdDSAKeys {
		t.Fatalf("%d Keys on the replica, want %d", len(names), shape.ECDSAKeys+shape.EdDSAKeys)
	}
	for _, name := range names {
		if info, err := os.Stat(s.opts.Paths.ConversationWorkspace(s.opts.Alias, name)); err != nil || !info.IsDir() {
			t.Fatalf("Workspace %s after the restore: %v", name, err)
		}
	}
	if len(lines) != 0 {
		t.Fatalf("logged %q", lines)
	}
}

// A Workspace the watcher cannot create is retried on every wake but
// logged once, and once more when it succeeds.
func TestWorkspaceWatcherLogsAPersistentErrorOnce(t *testing.T) {
	var lines []string
	s, store, w := watcherOn(t, &lines)
	if err := store.Restore(coslbench.Build(t, coslbench.Shapes[0]).Path); err != nil {
		t.Fatal(err)
	}
	var one string
	for _, name := range workspaceNames(t, store) {
		one = s.opts.Paths.ConversationWorkspace(s.opts.Alias, name)
		break
	}
	// A file where the Workspaces' directory belongs: no Workspace can be made.
	parent := filepath.Dir(one)
	if err := os.MkdirAll(filepath.Dir(parent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w.wake()
	}
	if len(lines) != 1 {
		t.Fatalf("after three failing wakes logged %q, want one line", lines)
	}

	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	w.wake()
	w.wake()
	if len(lines) != 2 {
		t.Fatalf("after recovering logged %q, want one more line", lines)
	}
	if _, err := os.Stat(one); err != nil {
		t.Fatalf("Workspace after recovering: %v", err)
	}
}
