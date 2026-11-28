package config

import (
	"fmt"
	"slices"
	"strings"
)

// Tier is what an Agent Profile's tiered Tools act on, which fixes where
// it may be included (ADR 0046).
type Tier int

const (
	// TierNone: it grants no tiered Tool, so may be included anywhere.
	TierNone Tier = iota
	// TierKey: it grants Key Tools, so is included only in a Key's
	// Conversation.
	TierKey
	// TierGroup: it grants Group Tools, so is included only in Group-level
	// talk.
	TierGroup
)

// KeyTools act on one Key: the Key of the Conversation they run in.
var KeyTools = []string{ToolSignList, ToolSignInspect, ToolSignPropose, ToolSignAccept, ToolSignRefuse, ToolChainFill, ToolKeyAddress}

// GroupTools act on the Group as a whole. The talk_ Tools and skill_load,
// in neither list, have no tier.
var GroupTools = []string{ToolJobList, ToolJobAdd, ToolJobEdit, ToolJobDelete, ToolHookList, ToolHookAdd, ToolHookEdit, ToolHookDelete,
	ToolScheduleList, ToolScheduleAdd, ToolScheduleEdit, ToolScheduleDelete}

// String names t as a refusal does: Key, Group or untiered.
func (t Tier) String() string {
	switch t {
	case TierKey:
		return "Key"
	case TierGroup:
		return "Group"
	}
	return "untiered"
}

// Fits reports whether a profile of tier t may be included in Group-level
// talk, groupTalk, or else in a Key's Conversation.
func (t Tier) Fits(groupTalk bool) bool {
	switch t {
	case TierKey:
		return !groupTalk
	case TierGroup:
		return groupTalk
	}
	return true
}

// Where says where a profile of tier t may be included.
func (t Tier) Where() string {
	switch t {
	case TierKey:
		return "only in a Key's Conversation"
	case TierGroup:
		return "only in Group-level talk"
	}
	return "anywhere"
}

// ToolTier is the tier of the built-in Tool name, TierNone for an untiered
// one.
func ToolTier(name string) Tier {
	switch {
	case slices.Contains(KeyTools, name):
		return TierKey
	case slices.Contains(GroupTools, name):
		return TierGroup
	}
	return TierNone
}

// Tier is p's tier, that of the tiered Tools it grants. A profile granting
// Tools of both tiers has none, and is an error naming them.
func (p Profile) Tier() (Tier, error) {
	var keys, groups []string
	for _, t := range p.Tools {
		switch ToolTier(t) {
		case TierKey:
			keys = append(keys, t)
		case TierGroup:
			groups = append(groups, t)
		}
	}
	switch {
	case len(keys) > 0 && len(groups) > 0:
		return TierNone, fmt.Errorf("tools: grants Key Tools (%s) and Group Tools (%s), but a profile grants Tools of one tier only: no agent that answers in a Key's Conversation may change Triggers, and none in Group-level talk may sign",
			strings.Join(keys, ", "), strings.Join(groups, ", "))
	case len(keys) > 0:
		return TierKey, nil
	case len(groups) > 0:
		return TierGroup, nil
	}
	return TierNone, nil
}
