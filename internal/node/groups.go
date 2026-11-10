package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"defios.com/internal/config"
	"defios.com/internal/cosl"
	"defios.com/internal/group"
	"defios.com/internal/identity"
	"defios.com/internal/secret"
	"defios.com/internal/serve"
	"defios.com/internal/vault"
)

// ErrGroupExists: the Node already has a Group by that alias.
var ErrGroupExists = errors.New("node: a Group by that alias already exists on this Node")

// TailnetAuthKeySecret is the Secret a new Group's tailnet_auth_key names.
const TailnetAuthKeySecret = "TS_AUTHKEY"

// GroupSpec is what NewGroup scaffolds.
type GroupSpec struct {
	Alias        string // this Node's local name for the Group
	HeadscaleURL string // the Group's tailnet control server
	// Hostname is the tsnet hostname; "" is defios-<first 8 hex of the
	// Node ID>-<alias>.
	Hostname string
	// DisplayName is this Node's name in the Group; "" is the Node's
	// default display name.
	DisplayName string
}

// GroupConfig is a scaffolded Group's config as NewGroup wrote it.
type GroupConfig struct {
	Alias, DisplayName, Hostname, HeadscaleURL string
}

// Group statuses GroupSummary reports besides group.Status's pending and
// valid.
const (
	StatusUnformed = "unformed" // scaffolded, no Record yet
	StatusError    = "error"    // a Record that no longer verifies
)

// GroupSummary is one of the Node's Groups as a list shows it.
type GroupSummary struct {
	Alias   string
	Status  string // unformed | pending | valid | error
	GroupID string // "" until formed
	Served  bool   // this Node serves it now, in this process or another
}

// GroupDetail is one Group with its Roster.
type GroupDetail struct {
	Alias   string
	GroupID string
	Status  group.Status
	Roster  []GroupNode
}

// GroupNode is one Roster Node as GroupDetail shows it. Reachability and
// refreshed names come from live Identities while the Group is served;
// the pinned keys never do (§6.2).
type GroupNode struct {
	NodeID              string
	DisplayName         string
	Hostname            string
	Reachable           bool
	Joined              bool   // its membership.join is on the COSL
	AuthPublicKey       string // hex
	EncryptionPublicKey string // hex
}

// NewGroup scaffolds a Group under a local alias: its config and data
// subtrees and its config.toml, before any network (ADR 0007). Its
// tailnet_auth_key names the TailnetAuthKeySecret, for SetSecret.
func (h *Host) NewGroup(spec GroupSpec) (GroupConfig, error) {
	if err := group.ValidateAlias(spec.Alias); err != nil {
		return GroupConfig{}, &InvalidError{Err: err}
	}
	if spec.HeadscaleURL == "" {
		return GroupConfig{}, invalidf("a headscale URL is required")
	}
	if _, err := os.Stat(h.p.GroupConfigFile(spec.Alias)); err == nil {
		return GroupConfig{}, fmt.Errorf("%w: %q", ErrGroupExists, spec.Alias)
	} else if !errors.Is(err, os.ErrNotExist) {
		return GroupConfig{}, err
	}
	name := spec.DisplayName
	if name == "" {
		raw, err := os.ReadFile(h.p.DisplayNameFile())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return GroupConfig{}, err
		}
		name = strings.TrimSpace(string(raw))
	}
	if err := ValidateDisplayName(name); err != nil {
		return GroupConfig{}, err
	}
	hostname := spec.Hostname
	if hostname == "" {
		hostname = "defios-" + h.NodeID()[:8] + "-" + spec.Alias
	}
	for _, dir := range []string{h.p.GroupConfigDir(spec.Alias), h.p.GroupDir(spec.Alias)} {
		if err := mkdirPrivate(dir); err != nil {
			return GroupConfig{}, err
		}
	}
	cfg := config.Config{
		HeadscaleURL:   spec.HeadscaleURL,
		DisplayName:    name,
		Hostname:       hostname,
		TailnetAuthKey: "$" + TailnetAuthKeySecret,
		Jobs:           config.DefaultJobs(),
	}
	if err := config.WriteFile(h.p.GroupConfigFile(spec.Alias), cfg); err != nil {
		return GroupConfig{}, err
	}
	return GroupConfig{Alias: spec.Alias, DisplayName: name, Hostname: hostname, HeadscaleURL: spec.HeadscaleURL}, nil
}

// SetSecret seals value into group's Secrets under name, replacing any
// value it had. A served Group reads it at its next use.
func (h *Host) SetSecret(groupAlias, name, value, passphrase string) error {
	if err := secret.ValidateName(name); err != nil {
		return &InvalidError{Err: err}
	}
	if value == "" {
		return invalidf("a Secret must not be empty")
	}
	if err := h.knownGroup(groupAlias); err != nil {
		return err
	}
	unlock, err := useVault(h.p)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := vault.Open(h.p.VaultSharedFile(), passphrase); err != nil {
		return err
	}
	path := h.p.VaultSecretsFile(groupAlias)
	secrets, err := vault.OpenSecrets(path, passphrase)
	if errors.Is(err, os.ErrNotExist) {
		secrets = map[string]string{}
	} else if err != nil {
		return err
	}
	secrets[name] = value
	return vault.SealSecrets(path, secrets, passphrase)
}

// knownGroup refuses an alias the Node has no Group by.
func (h *Host) knownGroup(alias string) error {
	aliases, err := h.aliases()
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if a == alias {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownGroup, alias)
}

// aliases lists every Group on this Node: the alias subdirectories under
// the config and data trees, so an unformed Group is one. Sorted.
func (h *Host) aliases() ([]string, error) {
	seen := map[string]bool{}
	for _, root := range []string{h.p.GroupsConfigDir(), h.p.GroupsDir()} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				seen[e.Name()] = true
			}
		}
	}
	aliases := make([]string, 0, len(seen))
	for a := range seen {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return aliases, nil
}

// Groups lists the Node's Groups. One whose Record no longer verifies is
// listed with StatusError, so it never hides the rest.
func (h *Host) Groups(ctx context.Context) ([]GroupSummary, error) {
	aliases, err := h.aliases()
	if err != nil {
		return nil, err
	}
	served, _ := h.servedAlias(ctx)
	out := make([]GroupSummary, 0, len(aliases))
	for _, alias := range aliases {
		g := GroupSummary{Alias: alias, Status: StatusUnformed, Served: alias == served}
		switch rec, err := group.LoadAlias(h.p.GroupsDir(), alias); {
		case err == nil:
			g.Status, g.GroupID = rec.Status().String(), rec.GroupID().String()
		case errors.Is(err, group.ErrNoGroup):
		default:
			g.Status = StatusError
		}
		out = append(out, g)
	}
	return out, nil
}

// GroupByID is the alias of the Node's Group with this Group ID. Unformed
// Groups have none; a Record that fails to verify is reported as
// group.ErrBadProposal or group.ErrBadAccept.
func (h *Host) GroupByID(groupID string) (string, error) {
	gid, err := group.ParseID(strings.ToLower(groupID))
	if err != nil {
		return "", &InvalidError{Err: err}
	}
	aliases, err := h.aliases()
	if err != nil {
		return "", err
	}
	for _, alias := range aliases {
		rec, err := group.LoadAlias(h.p.GroupsDir(), alias)
		if errors.Is(err, group.ErrNoGroup) {
			continue
		} else if err != nil {
			return "", err
		}
		if rec.GroupID() == gid {
			return alias, nil
		}
	}
	return "", fmt.Errorf("%w: %s", group.ErrUnknownGroup, gid)
}

// InspectGroup is groupAlias's Record and Roster. An unformed Group is
// group.ErrNoGroup; a Record that no longer verifies is group.ErrBadProposal
// or group.ErrBadAccept.
func (h *Host) InspectGroup(ctx context.Context, groupAlias string) (GroupDetail, error) {
	rec, err := group.LoadAlias(h.p.GroupsDir(), groupAlias)
	if err != nil {
		return GroupDetail{}, err
	}
	joined, err := joinedNodes(h.p.CoslFile(groupAlias))
	if err != nil {
		return GroupDetail{}, err
	}
	live := map[string]DiscoveredNode{}
	served := false
	if api, err := h.formation(ctx, groupAlias); err == nil {
		served = true
		if nodes, err := discover(ctx, api); err == nil {
			for _, n := range nodes {
				live[n.NodeID] = n
			}
		}
	}
	d := GroupDetail{Alias: groupAlias, GroupID: rec.GroupID().String(), Status: rec.Status()}
	for _, n := range rec.Roster().Nodes() {
		row := GroupNode{
			NodeID:              n.NodeID.String(),
			DisplayName:         n.DisplayName,
			Hostname:            n.TsnetHostname,
			Joined:              joined[n.NodeID],
			AuthPublicKey:       hex.EncodeToString(n.AuthPublicKey),
			EncryptionPublicKey: hex.EncodeToString(n.EncryptionPublicKey),
		}
		if n.NodeID == h.pub.NodeID {
			row.Reachable = served
		} else if l, ok := live[row.NodeID]; ok && l.AuthPublicKey == row.AuthPublicKey && l.EncryptionPublicKey == row.EncryptionPublicKey {
			row.Reachable = true
			row.DisplayName, row.Hostname = l.DisplayName, l.Hostname
		}
		d.Roster = append(d.Roster, row)
	}
	return d, nil
}

// joinedNodes reads which Nodes have a membership.join on the COSL. No
// replica yet means nobody has.
func joinedNodes(coslPath string) (map[identity.ID]bool, error) {
	joined := map[identity.ID]bool{}
	if _, err := os.Stat(coslPath); errors.Is(err, os.ErrNotExist) {
		return joined, nil
	} else if err != nil {
		return nil, err
	}
	store, err := cosl.Open(coslPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.Log(cosl.Query{MsgTypes: []uint32{cosl.TypeMembershipJoin}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		joined[r.Sender] = true
	}
	return joined, nil
}

// servedAlias is the Group this Node serves now, by this host or by
// another process on its socket; ok false when none.
func (h *Host) servedAlias(ctx context.Context) (alias string, ok bool) {
	if cur := h.serving(); cur != nil {
		return cur.group, true
	}
	if !serve.IsUp(h.p.Socket) {
		return "", false
	}
	st, err := serve.Client{Socket: h.p.Socket}.Status(ctx)
	if err != nil {
		return "", false
	}
	return st.Alias, true
}
