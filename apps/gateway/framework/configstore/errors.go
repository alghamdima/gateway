package configstore

import (
	"errors"
	"fmt"
	"strings"
)

var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")

// ErrRoutingRulePriorityTaken is returned when a routing rule's priority is already used by another
// rule in the same scope and scope ID. It wraps ErrAlreadyExists and reads the same, so callers that
// only ask whether something already exists are unaffected; callers that advise changing the
// priority check for it, since a duplicate name surfaces as ErrAlreadyExists too.
var ErrRoutingRulePriorityTaken = fmt.Errorf("%w", ErrAlreadyExists)

// ErrDatasheetPricingClaimTaken is returned when a datasheet override's pricing
// patch targets a pricing row another override already patches (for example
// bedrock and bedrock_mantle, which share pricing rows). It wraps
// ErrAlreadyExists.
var ErrDatasheetPricingClaimTaken = fmt.Errorf("pricing for this datasheet row is already set by another override: %w", ErrAlreadyExists)

// ErrConfigUnreadable marks a stored configuration value that could not be
// decoded or that failed validation — the value is present but this version
// cannot make sense of it.
//
// It exists so callers can tell that apart from the store itself being
// unreachable. A caller with usable defaults should degrade on this and only
// this: degrading on every error would report an unreachable database as a
// working installation running on defaults, which is a worse lie than an error.
var ErrConfigUnreadable = errors.New("stored configuration is unreadable")

// ErrUnresolvedKeys is returned when one or more keys could not be resolved
type ErrUnresolvedKeys struct {
	Identifiers []string
}

func (e *ErrUnresolvedKeys) Error() string {
	return fmt.Sprintf("could not resolve keys: %s", strings.Join(e.Identifiers, ", "))
}

// ErrOAuthConfigChanged is returned when an oauth config was rebound by another caller first.
var ErrOAuthConfigChanged = errors.New("oauth config was changed concurrently")
