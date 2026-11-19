package transaction

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tinypell3ts/tiny-bank/pkg/outbox"
	transactionv1 "github.com/tinypell3ts/tiny-bank/proto/transaction/v1"
)

// TransfersTopic is the Kafka topic the service's events go to
// (deploy/kafka/topics.yaml).
const TransfersTopic = "transaction.transfers"

// AnnounceTransferChanged is the store's Announce: it saves a
// TransferChanged event for t, as it is just after a change, in the outbox,
// in tx, so the event is sent if and only if the change is saved. It's keyed
// by the transfer's ID, so one transfer's events stay in order. The outbox
// relay sends it.
//
// The event carries the trace in ctx: the request's for a transfer created
// or finished by a request, and the finisher's "finish transfer" trace for
// one the finisher finished later (that trace's transfer.created_trace_id
// leads back to the request).
func AnnounceTransferChanged(ctx context.Context, tx pgx.Tx, t Transfer) error {
	return outbox.Add(ctx, tx, TransfersTopic, []byte(t.ID.String()), &transactionv1.TransferChanged{
		EventId:    uuid.New().String(),
		OccurredAt: timestamppb.New(t.UpdatedAt),
		Transfer:   transferToProto(t),
	})
}
