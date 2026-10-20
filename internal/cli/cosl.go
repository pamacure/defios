package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"defios.com/internal/cosl"
	"defios.com/internal/identity"
	coslv1 "defios.com/proto/cosl/v1"
)

func coslCommand(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "cosl",
		Short: "Read this Node's COSL replica",
	}

	var (
		typeFlag string
		limit    int
	)
	log := &cobra.Command{
		Use:   "log",
		Short: "List COSL rows: seq, time, sender, type, payload summary",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return runCoslLog(g, typeFlag, limit) },
	}
	log.Flags().StringVar(&typeFlag, "type", "", "only this registry type (name or id)")
	log.Flags().IntVar(&limit, "limit", 0, "only the latest N rows")

	decode := &cobra.Command{
		Use:   "decode <msg-id>",
		Short: "Print one row's full envelope and decoded payload",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return runCoslDecode(g, args[0]) },
	}

	root.AddCommand(log, decode)
	return root
}

// openReplica finds this Node's one Group and opens its COSL. Reads work
// without serve: WAL readers see the FSM's committed rows.
func openReplica(g *globals) (*cosl.Store, error) {
	p := g.paths()
	matches, err := filepath.Glob(filepath.Join(p.GroupsDir(), "*", "cosl.sqlite"))
	if err != nil {
		return nil, exitOperational(err)
	}
	switch len(matches) {
	case 0:
		if _, err := os.Stat(p.Data); errors.Is(err, os.ErrNotExist) {
			return nil, errNoNode(p)
		}
		return nil, exitOperationalf("this Node has no COSL replica yet; it is created once a Group is valid and Raft starts")
	case 1:
	default:
		return nil, exitOperationalf("found %d Groups under %s; a Node has at most one", len(matches), p.GroupsDir())
	}
	store, err := cosl.Open(matches[0])
	if err != nil {
		return nil, exitOperational(err)
	}
	return store, nil
}

// displayNames maps sender Node IDs to the display names their
// membership.join rows carry. Later joins win.
func displayNames(store *cosl.Store) (map[identity.ID]string, error) {
	typ := cosl.TypeMembershipJoin
	rows, err := store.Log(cosl.Query{MsgType: &typ})
	if err != nil {
		return nil, err
	}
	names := map[identity.ID]string{}
	for _, r := range rows {
		env, err := r.Decode()
		if err != nil {
			continue
		}
		var join coslv1.MembershipJoin
		if proto.Unmarshal(env.Payload, &join) == nil && join.DisplayName != "" {
			names[r.Sender] = join.DisplayName
		}
	}
	return names, nil
}

func senderName(names map[identity.ID]string, sender identity.ID) string {
	if n, ok := names[sender]; ok {
		return n
	}
	return sender.String()[:8]
}

// parseType accepts a registry name or a numeric id.
func parseType(s string) (uint32, error) {
	if id, ok := cosl.TypeByName(s); ok {
		return id, nil
	}
	if n, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(n), nil
	}
	return 0, exitUsagef("--type %q is neither a registry name nor an id", s)
}

func rfc3339(unixNanos int64) string {
	return time.Unix(0, unixNanos).UTC().Format(time.RFC3339)
}

// logLine is one `cosl log` row, human or JSON.
type logLine struct {
	Seq               uint64  `json:"seq"`
	Term              uint64  `json:"term"`
	CreatedAt         string  `json:"created_at"`
	Sender            string  `json:"sender"`
	SenderDisplayName string  `json:"sender_display_name"`
	MsgType           uint32  `json:"msg_type"`
	TypeName          string  `json:"type_name"`
	MsgID             string  `json:"msg_id"`
	ConversationID    *string `json:"conversation_id"`
	Summary           string  `json:"summary"`
}

func runCoslLog(g *globals, typeFlag string, limit int) error {
	if limit < 0 {
		return exitUsagef("--limit must be >= 0")
	}
	q := cosl.Query{Limit: limit}
	if typeFlag != "" {
		id, err := parseType(typeFlag)
		if err != nil {
			return err
		}
		q.MsgType = &id
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()

	names, err := displayNames(store)
	if err != nil {
		return exitOperational(err)
	}
	rows, err := store.Log(q)
	if err != nil {
		return exitOperational(err)
	}

	lines := make([]logLine, 0, len(rows))
	for _, r := range rows {
		env, err := r.Decode()
		if err != nil {
			return exitOperational(err)
		}
		l := logLine{
			Seq:               r.Seq,
			Term:              r.Term,
			CreatedAt:         rfc3339(r.CreatedAt),
			Sender:            r.Sender.String(),
			SenderDisplayName: senderName(names, r.Sender),
			MsgType:           r.MsgType,
			TypeName:          cosl.TypeName(r.MsgType),
			MsgID:             hex.EncodeToString(r.MsgID[:]),
			Summary:           cosl.Summary(r.MsgType, env.Payload),
		}
		if r.ConversationID != nil {
			c := hex.EncodeToString(r.ConversationID)
			l.ConversationID = &c
		}
		lines = append(lines, l)
	}
	if g.json {
		return g.emitJSON(lines)
	}
	for _, l := range lines {
		fmt.Fprintf(g.app.Stdout, "%d %s %s  %s  %s\n", l.Seq, l.CreatedAt, l.SenderDisplayName, l.TypeName, l.Summary)
	}
	return nil
}

// decodedRow is `cosl decode`'s view: row bookkeeping, every envelope
// field, and the payload decoded through the registry.
type decodedRow struct {
	Seq            uint64         `json:"seq"`
	Term           uint64         `json:"term"`
	PrevHash       string         `json:"prev_hash"`
	Version        uint32         `json:"version"`
	GroupID        string         `json:"group_id"`
	MsgType        uint32         `json:"msg_type"`
	TypeName       string         `json:"type_name"`
	Sender         string         `json:"sender"`
	MsgID          string         `json:"msg_id"`
	ConversationID *string        `json:"conversation_id"`
	CreatedAt      string         `json:"created_at"`
	Sig            string         `json:"sig"`
	Payload        map[string]any `json:"payload"`               // decoded registry message
	PayloadHex     string         `json:"payload_hex,omitempty"` // only when undecodable
}

func runCoslDecode(g *globals, arg string) error {
	prefix, err := hex.DecodeString(strings.ToLower(arg))
	if err != nil || len(prefix) == 0 {
		return exitUsagef("msg-id must be hex (a full 32-byte msg_id or a unique prefix)")
	}
	store, err := openReplica(g)
	if err != nil {
		return err
	}
	defer store.Close()

	row, err := store.ByMsgIDPrefix(prefix)
	switch {
	case errors.Is(err, cosl.ErrNotFound):
		return exitOperationalf("no COSL row with msg_id %s…", arg)
	case errors.Is(err, cosl.ErrAmbiguous):
		return exitUsagef("msg_id prefix %s matches more than one row", arg)
	case err != nil:
		return exitOperational(err)
	}
	env, err := row.Decode()
	if err != nil {
		return exitOperational(err)
	}

	d := decodedRow{
		Seq:       row.Seq,
		Term:      row.Term,
		PrevHash:  hex.EncodeToString(row.PrevHash[:]),
		Version:   env.Version,
		GroupID:   hex.EncodeToString(env.GroupId),
		MsgType:   env.MsgType,
		TypeName:  cosl.TypeName(env.MsgType),
		Sender:    hex.EncodeToString(env.Sender),
		MsgID:     hex.EncodeToString(env.MsgId),
		CreatedAt: rfc3339(env.CreatedAt),
		Sig:       hex.EncodeToString(env.Sig),
	}
	if env.ConversationId != nil {
		c := hex.EncodeToString(env.ConversationId)
		d.ConversationID = &c
	}
	var fields []cosl.Field
	if msg, err := cosl.DecodePayload(env.MsgType, env.Payload); err == nil {
		fields = cosl.Fields(msg)
		d.Payload = cosl.FieldsToMap(fields)
	} else {
		d.PayloadHex = hex.EncodeToString(env.Payload)
	}
	if g.json {
		return g.emitJSON(d)
	}

	w := g.app.Stdout
	conv := "(unset: Group-level)"
	if d.ConversationID != nil {
		conv = *d.ConversationID
	}
	fmt.Fprintf(w, "seq:             %d\n", d.Seq)
	fmt.Fprintf(w, "term:            %d\n", d.Term)
	fmt.Fprintf(w, "prev_hash:       %s\n", d.PrevHash)
	fmt.Fprintf(w, "version:         %d\n", d.Version)
	fmt.Fprintf(w, "group_id:        %s\n", d.GroupID)
	fmt.Fprintf(w, "msg_type:        %d (%s)\n", d.MsgType, d.TypeName)
	fmt.Fprintf(w, "sender:          %s\n", d.Sender)
	fmt.Fprintf(w, "msg_id:          %s\n", d.MsgID)
	fmt.Fprintf(w, "conversation_id: %s\n", conv)
	fmt.Fprintf(w, "created_at:      %s\n", d.CreatedAt)
	fmt.Fprintf(w, "sig:             %s\n", d.Sig)
	if fields == nil {
		fmt.Fprintf(w, "payload:         %s\n", d.PayloadHex)
		return nil
	}
	fmt.Fprintln(w, "payload:")
	printFields(w, fields, "  ")
	return nil
}

func printFields(w io.Writer, fields []cosl.Field, indent string) {
	for _, f := range fields {
		switch v := f.Value.(type) {
		case []cosl.Field:
			fmt.Fprintf(w, "%s%s:\n", indent, f.Name)
			printFields(w, v, indent+"  ")
		case []any:
			fmt.Fprintf(w, "%s%s: [%d]\n", indent, f.Name, len(v))
			for _, item := range v {
				if nested, ok := item.([]cosl.Field); ok {
					fmt.Fprintf(w, "%s  -\n", indent)
					printFields(w, nested, indent+"    ")
				} else {
					fmt.Fprintf(w, "%s  - %v\n", indent, item)
				}
			}
		default:
			fmt.Fprintf(w, "%s%s: %v\n", indent, f.Name, v)
		}
	}
}
