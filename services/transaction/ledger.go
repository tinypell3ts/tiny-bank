package transaction

import (
	"context"
	"fmt"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/funding"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	ledgerv1 "github.com/tinypell3ts/tiny-bank/proto/ledger/v1"
)

// Ledger posts a transfer's journal and returns the journal's ID. The ledger
// service answers it (LedgerService); tests use a fake.
type Ledger interface {
	Post(ctx context.Context, t Transfer) (journalID uuid.UUID, err error)
}

// LedgerService posts to the ledger at BaseURL (http://ledger on the
// cluster), through its contract, proto/ledger/v1, in binary protobuf.
type LedgerService struct {
	BaseURL string
}

// Post sends t's journal to POST /journals, with the transfer's own ID as
// the idempotency key. So however many times a transfer is posted, by
// whichever attempt, the ledger moves its money once, and a repeat gets the
// first journal back.
func (l LedgerService) Post(ctx context.Context, t Transfer) (uuid.UUID, error) {
	req, err := journalFor(t)
	if err != nil {
		return uuid.Nil(), err
	}
	var j ledgerv1.Journal
	if err := kit.Post(ctx, l.BaseURL+"/journals", req, &j); err != nil {
		return uuid.Nil(), err
	}
	id, ok := kit.ParseUUID(j.GetId())
	if !ok {
		return uuid.Nil(), terrors.BadResponse("journal_id", "the ledger's journal has no valid ID", nil)
	}
	return id, nil
}

// journalFor is the ledger journal that carries out t: from its account to
// its to account, or, for a deposit, from the currency's funding account.
func journalFor(t Transfer) (*ledgerv1.PostJournalRequest, error) {
	from := t.From
	if t.Kind == KindDeposit {
		var ok bool
		if from, ok = funding.Account(t.Amount.Currency()); !ok {
			return nil, fmt.Errorf("deposit %s: no funding account for %s", t.ID, t.Amount.Currency())
		}
	}
	return &ledgerv1.PostJournalRequest{
		IdempotencyKey: t.ID.String(),
		Currency:       t.Amount.Currency().Code(),
		Entries: []*ledgerv1.Entry{
			{AccountId: from.String(), Amount: -t.Amount.Amount()},
			{AccountId: t.To.String(), Amount: t.Amount.Amount()},
		},
	}, nil
}

// outcome is what a post's answer means for a transfer.
type outcome int

const (
	// posted: the ledger posted it, so the money moved.
	posted outcome = iota
	// refusedForGood: the ledger refused it with a 4xx a retry can't change,
	// like insufficient funds or an account that isn't open.
	refusedForGood
	// keyReused: the ledger says this transfer's key was used for a different
	// journal. A transfer's journal is built the same way every time, so this
	// is a bug: never retried, and never marked failed, as the money may have
	// moved under the other journal.
	keyReused
	// unsure: a timeout, a 5xx, no connection, an answer that didn't make
	// sense. The money may or may not have moved, so try again, same key.
	unsure
)

// outcomeOf says what err, the ledger's answer to a post, means, and for a
// refusal, its terror code to record on the failed transfer. It decides
// refusals with kit.Refusal, as the ledger's own metrics do, so the two
// services agree on what a refusal is.
func outcomeOf(err error) (outcome, string) {
	if err == nil {
		return posted, ""
	}
	code, ok := kit.Refusal(err)
	switch {
	case ok && code == "precondition_failed.idempotency_key":
		return keyReused, ""
	case ok:
		return refusedForGood, code
	}
	return unsure, ""
}
