package shell

import (
	"sort"

	"defios.com/internal/node"
	"defios.com/internal/secret"
)

// The shared Secret field's part: the edit-target Group's Secrets, and
// "New Secret…", for any page with a field that takes a Secret.

// SecretsView is the edit-target Group's Secrets as the shared Secret
// field offers them: each by name with where it resolves from.
type SecretsView struct {
	Group   string
	Secrets []SecretName
	Problem *Problem
	// Field is "name" or "value" for a refused NewSecret; Set the name
	// it set.
	Field string
	Set   string
}

// SecretName is a Secret the field offers: secret.SourceEnv, SourceVault
// or SourceNone (named by a config field, with no value anywhere).
type SecretName struct {
	Name, Source string
}

// Secrets lists the edit-target Group's Secrets for the shared Secret
// field.
func (s *Shell) Secrets() SecretsView {
	v, ok := s.secretsTarget()
	if !ok {
		return v
	}
	s.fillSecrets(&v)
	return v
}

// NewSecret keeps value in the edit-target Group's Vault as Secret name,
// for the shared Secret field's "New Secret…".
func (s *Shell) NewSecret(name, value string) SecretsView {
	s.op.Lock()
	defer s.op.Unlock()
	v, ok := s.secretsTarget()
	if !ok {
		return v
	}
	if v.Field, v.Problem = s.setSecret(v.Group, name, value); v.Problem == nil {
		v.Set = name
	}
	s.fillSecrets(&v)
	return v
}

// setSecret keeps value in group's Vault as Secret name. A refusal names
// the field it is about: "name" or "value".
func (s *Shell) setSecret(group, name, value string) (field string, p *Problem) {
	switch {
	case secret.ValidateName(name) != nil:
		return "name", &Problem{"invalid", "A Secret's name is capital letters, digits and _, starting with a letter, such as ANTHROPIC_API_KEY."}
	case value == "":
		return "value", &Problem{"invalid", "Enter the Secret's value."}
	}
	if err := s.node().SetSecret(s.ctx, group, name, value, s.held()); err != nil {
		return "value", problemOf(err)
	}
	return "", nil
}

func (s *Shell) secretsTarget() (SecretsView, bool) {
	a, ok := s.agentsTarget()
	return SecretsView{Group: a.Group}, ok
}

func (s *Shell) fillSecrets(v *SecretsView) {
	list, err := s.node().Secrets(s.ctx, v.Group, s.held())
	if err != nil {
		if v.Problem == nil {
			v.Problem = problemOf(err)
		}
		return
	}
	v.Secrets = make([]SecretName, len(list))
	for i, sc := range list {
		v.Secrets[i] = SecretName{Name: sc.Name, Source: sc.Source}
	}
	sort.Slice(v.Secrets, func(i, j int) bool { return v.Secrets[i].Name < v.Secrets[j].Name })
}

// held is the passphrase the app holds, for the Vault.
func (s *Shell) held() node.Passphrase {
	s.mu.Lock()
	pass := s.pass
	s.mu.Unlock()
	return func() (string, bool, error) { return pass, pass != "", nil }
}
