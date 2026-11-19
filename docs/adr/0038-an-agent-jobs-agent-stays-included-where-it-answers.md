# An agent Job's agent stays included where it answers

An agent Job can name the place it answers in: Group-level talk, or a Key's
Conversation by its Workspace name. Its agent answers only where its Handle is
included. Before this, a Job whose agent was not included there was caught
only for `target = "group"`, when the config was read. For a Key's
Conversation it was caught only when the Job ran and failed, and an exclude
made later could break a Job that had worked.

`internal/node` now keeps the two in step, for every front end:

- **AddJob and EditJob** refuse an agent Job whose agent is not included in
  the place it names (`JobNotIncludedError`). For a Conversation, that is its
  own list, or the Group's if it follows the Group's.
- **IncludeAgent, ExcludeAgent and SetIncludedAgents** refuse a change that
  would take a Job's agent out of the place that Job answers in
  (`AnswersError`, naming each such Job). An exclude from Group-level talk
  also counts for the Conversations that follow its list. So does an include
  that gives a following Conversation its own list of that Handle alone.
- Only changes that newly break a Job are refused. A Job that is already
  broken, say by a hand edit, does not block unrelated changes.

A Job that answers in the event's Conversation names no place, so it can only
be checked when it runs. Editing `config.toml` as text is still checked only
by the loader, as before.

## Considered options

- **Including the agent automatically when a Job is saved.** Including an
  agent is a grant made on Talk: it makes the agent summonable there and lets
  it read the talk. Saving a Job would grant it as a side effect, and deleting
  the Job would not take it back. Instead, the desktop form marks each place
  the agent is not included in, and offers a separate "Include @handle there"
  action that leaves the Job unsaved.
- **Leaving those places out of the form's list.** That gives no reason why a
  place is missing, and no way to fix it from the form.
