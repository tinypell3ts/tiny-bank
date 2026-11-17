package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"uuid"

	"github.com/monzo/terrors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/funding"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	accountv1 "github.com/tinypell3ts/tiny-bank/proto/account/v1"
)

// Service is the ledger: the Store, plus the account checks a post needs. It
// has the Store's Journal and Balance, and its own Post.
type Service struct {
	*Store
	accounts Accounts
	metrics  *metrics
}

// NewService returns the ledger, storing journals in store, checking customer
// accounts with accounts, and registering its business metrics on reg.
func NewService(store *Store, accounts Accounts, reg prometheus.Registerer) *Service {
	return &Service{Store: store, accounts: accounts, metrics: newMetrics(reg)}
}

// Post moves the money in req, as Store.Post does, once every customer
// account in it is checked with the account service: it must exist, be open
// and hold req's currency. Funding accounts are the ledger's own, so they
// aren't checked.
//
// A retry, a request whose idempotency key is already used, gets the journal
// posted the first time without any checks. Its money has already moved, so
// refusing it now (because an account has closed since, say) would tell the
// caller it hadn't.
//
// The checks run before Store.Post takes any locks or opens a transaction, so
// a slow account service never holds up other posts on the same accounts.
// An account could change between its check and the post; while accounts
// can't close, that can't matter.
func (s *Service) Post(ctx context.Context, req NewJournal) (j Journal, created bool, err error) {
	defer func() { s.metrics.record(req, j, created, err) }()
	return s.checkAndPost(ctx, req)
}

// CountRefusal counts a post the HTTP handler refused before it reached Post.
func (s *Service) CountRefusal(currency money.Currency, err error) {
	s.metrics.refused(currency, err)
}

// checkAndPost validates req, answers a retry with its first journal, checks
// the accounts, and posts. Post wraps it so every return is counted.
func (s *Service) checkAndPost(ctx context.Context, req NewJournal) (Journal, bool, error) {
	req, err := req.Validate()
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	j, found, err := s.posted(ctx, req)
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	if found {
		return j, false, nil
	}
	if err := checkAccounts(ctx, s.accounts, req.Entries); err != nil {
		// Another request with the same key may have posted while these checks
		// ran: it passed them, and this one's lookup timed out, say. Its money
		// has moved, so this request is a retry of it and gets its journal,
		// not an error that would say nothing moved.
		if j, found, lookupErr := s.posted(ctx, req); lookupErr == nil && found {
			return j, false, nil
		}
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	return s.postValid(ctx, req)
}

// checkAccounts checks every customer account in entries at once, so a post
// waits for one lookup's time (at most kit.Get's 5 seconds), not one per
// account. If several fail, the error is the first entry's, in account ID
// order, so the same request always gets the same answer.
func checkAccounts(ctx context.Context, accounts Accounts, entries []Entry) error {
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		if funding.Is(e.AccountID) {
			continue
		}
		wg.Go(func() { errs[i] = checkAccount(ctx, accounts, e) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func checkAccount(ctx context.Context, accounts Accounts, e Entry) error {
	info, err := accounts.Account(ctx, e.AccountID)
	if err != nil {
		return err
	}
	if !info.Open {
		return errAccountNotOpen(e.AccountID)
	}
	if info.Currency != e.Amount.Currency() {
		return errWrongCurrency(e.AccountID, info.Currency)
	}
	return nil
}

// Accounts is what the ledger needs to know about customer accounts. The
// account service answers it (AccountService); tests use a fake.
type Accounts interface {
	// Account returns what the ledger needs to know about account id, or a
	// terrors not_found.account if it doesn't exist.
	Account(ctx context.Context, id uuid.UUID) (AccountInfo, error)
}

// AccountInfo is the part of an account the ledger checks before moving money.
type AccountInfo struct {
	Open     bool
	Currency money.Currency
}

// AccountService asks the account service at BaseURL (http://account on the
// cluster), through its contract, proto/account/v1. If a second service needs
// to look up accounts, this moves into a client package of its own.
type AccountService struct {
	BaseURL string
}

// Account looks id up with GET /accounts/:id.
//
// Only "it doesn't exist" passes on, as the ledger's own not_found.account.
// Anything else the account service says, or not being able to reach it, is
// the ledger's internal_service.account_service: a 500, with the account
// service's error kept as its cause, so kit.Handler logs it in full. The
// account service's own codes never reach the ledger's callers, who didn't
// call it. No money moves.
func (a AccountService) Account(ctx context.Context, id uuid.UUID) (AccountInfo, error) {
	var acc accountv1.Account
	if err := kit.Get(ctx, a.BaseURL+"/accounts/"+id.String(), &acc); err != nil {
		var terr *terrors.Error
		if errors.As(err, &terr) && terrors.PrefixMatches(terr, terrors.ErrNotFound) {
			return AccountInfo{}, errNoSuchAccount(id)
		}
		return AccountInfo{}, terrors.NewInternalWithCause(err, "couldn't check an account with the account service", nil, "account_service")
	}
	currency, err := money.ParseCurrency(acc.GetCurrency())
	if err != nil {
		return AccountInfo{}, fmt.Errorf("account %s from the account service: %w", id, err)
	}
	// Any status but open, including one this ledger doesn't know yet, means
	// "not open": the contract says to treat an unknown status that way.
	return AccountInfo{Open: acc.GetStatus() == accountv1.AccountStatus_ACCOUNT_STATUS_OPEN, Currency: currency}, nil
}
