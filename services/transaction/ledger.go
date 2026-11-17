package transaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// errBug is the ledger saying this transfer's key was used for a different
// journal. The transaction service builds a transfer's journal the same way
// every time, so this can only be a bug: it's never retried, and never
// marked failed (the money may have moved under the other journal).
var errBug = errors.New("the ledger says this transfer's key was used for a different journal: a bug")

// refused tells what the ledger's answer err means for a transfer:
//
//   - final: the ledger refused it for good (a 4xx that a retry can't change,
//     like insufficient funds or an account that isn't open), with reason,
//     the ledger's terror code, to record on the failed transfer
//   - errBug: the key was reused for a different journal (see errBug)
//   - otherwise (a timeout, a 5xx, no connection, an answer that didn't
//     make sense): we can't tell whether the money moved, so try again with
//     the same key
func refused(err error) (reason string, final bool, bug bool) {
	var terr *terrors.Error
	if !errors.As(err, &terr) {
		return "", false, false
	}
	if terr.Code == "precondition_failed.idempotency_key" {
		return "", false, true
	}
	switch strings.SplitN(terr.Code, ".", 2)[0] {
	case terrors.ErrBadRequest, terrors.ErrNotFound, terrors.ErrPreconditionFailed:
		return terr.Code, true, false
	}
	return "", false, false
}
