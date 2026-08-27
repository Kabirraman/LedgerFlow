package simulation

import (
	"fmt"
	"hash/fnv"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// World resolves what happens when a strategy contacts a customer.
//
// It is the reason this benchmark can compare strategies at all. Every customer
// is given a fixed sequence of latent propensities — how willing they are to pay
// on their first contact, their second, their third — derived from the dataset
// seed and the case id alone. An intervention succeeds when its effectiveness
// clears that contact's propensity.
//
// The consequence is the property that makes the comparison fair: two strategies
// that reach the same case on the same contact face the same threshold. They can
// only differ by choosing a more or less effective action, which is exactly the
// thing being measured. If the roll depended on the action, a strategy could win
// by being lucky in its choices rather than right.
//
// None of this is a claim about real customer behaviour. It is a stated model,
// versioned with the dataset, that every strategy is scored against identically
// (SRS 17.3, 25.2).
type World struct {
	seed int64
	// decay is how much each repeat of the same action loses. The third identical
	// reminder does not work as well as the first — that is the single most
	// important dynamic in recovery, and a model without it would reward a
	// strategy for simply contacting people more often.
	decay float64
}

// AttemptDecay is the multiplier applied per prior attempt of the same action
// type. It is fixed here, before any results exist, so it cannot be tuned to
// flatter a strategy afterwards (SRS 25.2).
const AttemptDecay = 0.6

// NewWorld builds the outcome model for a dataset seed.
func NewWorld(seed int64) World { return World{seed: seed, decay: AttemptDecay} }

// Outcome is one resolved attempt.
type Outcome struct {
	// Recovered is whether the money arrived.
	Recovered bool
	// Effectiveness is the decayed success probability the attempt was resolved
	// against, recorded so calibration error can be attributed to the strategy's
	// estimate rather than to the model's.
	Effectiveness float64
	// Propensity is the customer's threshold for this contact.
	Propensity float64
}

// Resolve decides whether one attempt recovers the case.
//
// contact is the 1-based index of this contact with the customer, across all
// action types; priorSameType is how many times this exact action has already
// been tried on this case. The first drives which propensity applies, the second
// drives decay.
func (w World) Resolve(c domain.BenchmarkCase, action domain.ActionType, contact, priorSameType int) Outcome {
	out := Outcome{Propensity: w.propensity(c.ID, contact)}

	// A non-external action moves no money by definition, and an unrecoverable
	// case has a zero curve, so both fall out here without consulting the roll.
	if !action.IsExternal() || !c.Recoverable {
		return out
	}

	base := c.RecoveryProbabilityByAction[action]
	if base <= 0 {
		return out
	}
	eff := base
	for i := 0; i < priorSameType; i++ {
		eff *= w.decay
	}
	out.Effectiveness = eff
	out.Recovered = out.Propensity < eff
	return out
}

// propensity is the customer's threshold for their n-th contact, in [0,1).
//
// It is a hash rather than a PRNG draw because it must be addressable: the
// harness needs the threshold for one specific (case, contact) pair without
// having replayed every other case first. That is what lets four strategies run
// in any order, or independently, and still face the identical world.
func (w World) propensity(caseID string, contact int) float64 {
	if contact < 1 {
		contact = 1
	}
	h := fnv.New64a()
	// Errors are impossible on a hash writer; ignoring them keeps the intent
	// readable rather than wrapping four writes in error checks that cannot fire.
	_, _ = fmt.Fprintf(h, "%d|%s|%d", w.seed, caseID, contact)
	// The low 53 bits are used so the division is exact in float64.
	const mantissa = 1 << 53
	return float64(h.Sum64()%mantissa) / float64(mantissa)
}

// reviewApproves decides whether the simulated reviewer approves an escalated
// case.
//
// LEDGERFLOW is a human-in-the-loop system by design: high-value and
// low-confidence cases are meant to reach a person. A benchmark that credited
// escalated revenue as unrecoverable would therefore be measuring the system
// with its main safety feature scored as a failure, and one that approved
// everything would be measuring a system that does not exist.
//
// So the reviewer is modelled explicitly: a fixed approval rate, applied
// deterministically per case, identical across strategies. Baselines never reach
// this path because they do not escalate — they act instead, which is what their
// policy-violation count reflects.
func (w World) reviewApproves(caseID string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s|review", w.seed, caseID)
	const mantissa = 1 << 53
	return float64(h.Sum64()%mantissa)/float64(mantissa) < rate
}
