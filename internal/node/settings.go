package node

import (
	"context"
	"errors"

	"defios.com/internal/config"
	"defios.com/internal/secret"
)

// GroupSettings is this Node's presence in a Group, as its config.toml
// sets it: how it joins the Group's tailnet and its prune policy. A served
// Group takes a change the next time it is served.
type GroupSettings struct {
	DisplayName  string // this Node's name in the Group
	HeadscaleURL string // the Group's tailnet control server
	Hostname     string // the tsnet hostname
	// TailnetAuthKey is a "$NAME" reference to the tailnet preauth key;
	// "" names none.
	TailnetAuthKey string
	// Prune is how old talk must be for its payload to be destroyed; 0 is
	// none, an archival Node.
	Prune config.Age
}

// SettingsEdit is a change to GroupSettings; a nil field is left as it is.
type SettingsEdit struct {
	DisplayName    *string
	HeadscaleURL   *string
	Hostname       *string
	TailnetAuthKey *string
	Prune          *config.Age
}

// GroupSettings is group's settings, the Group served or not.
func (h *Host) GroupSettings(ctx context.Context, group string) (GroupSettings, error) {
	c, err := h.groupConfig(group)
	if err != nil {
		return GroupSettings{}, err
	}
	return settingsOf(c), nil
}

// EditGroupSettings applies e to group's settings, each as NewGroup
// checks it, and returns them as written. A refused edit writes nothing.
func (h *Host) EditGroupSettings(ctx context.Context, group string, e SettingsEdit) (GroupSettings, error) {
	var out GroupSettings
	err := h.writeGroupConfig(group, func(c *config.Config) error {
		if e.DisplayName != nil {
			if err := ValidateDisplayName(*e.DisplayName); err != nil {
				return err
			}
			c.DisplayName = *e.DisplayName
		}
		if e.HeadscaleURL != nil {
			if *e.HeadscaleURL == "" {
				return invalidf("a headscale URL is required")
			}
			c.HeadscaleURL = *e.HeadscaleURL
		}
		if e.Hostname != nil {
			if *e.Hostname == "" {
				return invalidf("a hostname is required")
			}
			c.Hostname = *e.Hostname
		}
		if e.TailnetAuthKey != nil {
			if ref := *e.TailnetAuthKey; ref != "" {
				if _, err := secret.ParseRef(ref); err != nil {
					return invalidf("tailnet auth key: %w", err)
				}
			}
			c.TailnetAuthKey = *e.TailnetAuthKey
		}
		if e.Prune != nil {
			if *e.Prune < 0 {
				return invalidf("prune age %s must be positive, or 0 for none", *e.Prune)
			}
			c.Prune = *e.Prune
		}
		out = settingsOf(*c)
		return nil
	})
	if errors.Is(err, config.ErrNotWritten) {
		return GroupSettings{}, &InvalidError{Err: err}
	}
	if err != nil {
		return GroupSettings{}, err
	}
	return out, nil
}

func settingsOf(c config.Config) GroupSettings {
	return GroupSettings{
		DisplayName:    c.DisplayName,
		HeadscaleURL:   c.HeadscaleURL,
		Hostname:       c.Hostname,
		TailnetAuthKey: c.TailnetAuthKey,
		Prune:          c.Prune,
	}
}
