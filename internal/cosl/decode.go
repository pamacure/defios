package cosl

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	coslv1 "defios.com/proto/cosl/v1"
)

// Field is one decoded payload field, in schema order. Value is a string
// (bytes as hex), an integer, a bool, a []Field (nested message), or a
// []any of those.
type Field struct {
	Name  string
	Value any
}

// Fields flattens a decoded payload for display. Bytes are hex, as every
// id in the CLI is.
func Fields(msg proto.Message) []Field {
	return messageFields(msg.ProtoReflect())
}

func messageFields(m protoreflect.Message) []Field {
	var out []Field
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		out = append(out, Field{Name: string(fd.Name()), Value: fieldValue(fd, m.Get(fd))})
	}
	return out
}

func fieldValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if fd.IsList() {
		list := v.List()
		items := make([]any, list.Len())
		for i := range items {
			items[i] = scalarValue(fd, list.Get(i))
		}
		return items
	}
	return scalarValue(fd, v)
}

func scalarValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.BytesKind:
		return hex.EncodeToString(v.Bytes())
	case protoreflect.MessageKind:
		return messageFields(v.Message())
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind, protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return v.Uint()
	default:
		return v.Int()
	}
}

// FieldsToMap converts Fields to a JSON-friendly map (nested as needed).
func FieldsToMap(fields []Field) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		out[f.Name] = mapValue(f.Value)
	}
	return out
}

func mapValue(v any) any {
	switch x := v.(type) {
	case []Field:
		return FieldsToMap(x)
	case []any:
		items := make([]any, len(x))
		for i := range x {
			items[i] = mapValue(x[i])
		}
		return items
	default:
		return v
	}
}

// Summary is the one-line payload digest `cosl log` prints.
func Summary(msgType uint32, payload []byte) string {
	msg, err := DecodePayload(msgType, payload)
	if errors.Is(err, ErrUnknownType) {
		return fmt.Sprintf("%d bytes", len(payload))
	}
	if err != nil {
		return "undecodable payload"
	}
	switch p := msg.(type) {
	case *coslv1.MembershipJoin:
		return fmt.Sprintf("display_name=%s hostname=%s", oneLine(p.DisplayName), oneLine(p.TsnetHostname))
	case *coslv1.SessionStart:
		return fmt.Sprintf("session=%s curve=%s t=%d n=%d", short(p.SessionId), p.Curve, p.T, p.N)
	case *coslv1.SessionAbort:
		return fmt.Sprintf("session=%s reason=%s", short(p.SessionId), oneLine(p.Reason))
	case *coslv1.SessionComplete:
		return fmt.Sprintf("session=%s %s:%x", short(p.SessionId), p.Curve, p.PublicKey)
	case *coslv1.TssSession:
		to := "broadcast"
		if len(p.RoutingTag) > 0 {
			to = "to=" + short(p.RoutingTag)
		}
		return fmt.Sprintf("session=%s %s %d bytes", short(p.SessionId), to, len(p.Data))
	case *coslv1.AppFreeform:
		return oneLine(p.Text)
	}
	return ""
}

// short is the first 8 hex chars of an id.
func short(id []byte) string {
	h := hex.EncodeToString(id)
	if len(h) > 8 {
		h = h[:8]
	}
	return h
}

// oneLine escapes line breaks and truncates so a row never spans lines.
func oneLine(s string) string {
	s = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}
