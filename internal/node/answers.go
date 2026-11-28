package node

import "defios.com/internal/config"

// JobAnswer is an agent Job that answers in a set place (ADR 0038).
type JobAnswer = config.JobAnswer

// AnswersError: an include, exclude or change of a list of agents refused
// because it would take the agents of Jobs out of where they answer.
type AnswersError = config.AnswersError

// JobNotIncludedError: an agent Job refused because its agent is not
// included where it answers.
type JobNotIncludedError = config.JobNotIncludedError
