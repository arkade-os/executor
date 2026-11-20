package execution

import (
	"context"
	"errors"
	"fmt"
	"testing"

	arkv1 "github.com/arkade-os/arkd/api-spec/protobuf/gen/ark/v1"
	"github.com/arkade-os/executor/pkg/template"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRefusal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		cancelled bool
		refused   bool
		coinState bool
	}{
		{name: "invalid argument", err: status.Error(codes.InvalidArgument, "invalid"), refused: true},
		{name: "failed precondition", err: status.Error(codes.FailedPrecondition, "precondition"), refused: true},
		{name: "not found", err: status.Error(codes.NotFound, "missing"), refused: true},
		{name: "already exists", err: status.Error(codes.AlreadyExists, "exists"), refused: true},
		{name: "out of range", err: status.Error(codes.OutOfRange, "range"), refused: true},
		{name: "unavailable", err: status.Error(codes.Unavailable, "offline")},
		{name: "deadline", err: status.Error(codes.DeadlineExceeded, "timeout")},
		{name: "cancelled rpc", err: status.Error(codes.Canceled, "cancelled")},
		{name: "ordinary error", err: errors.New("offline")},
		{name: "cancelled context", err: status.Error(codes.InvalidArgument, "invalid"), cancelled: true},
		{name: "ineligible", err: fmt.Errorf("%w: broken", template.ErrIneligible), refused: true},
		{name: "unaffordable", err: fmt.Errorf("%w: %w: fee 100 exceeds the cap 0", template.ErrIneligible, template.ErrUnaffordable)},
		{name: "duplicate transaction", err: status.Error(codes.AlreadyExists, "arkd: duplicated offchain tx: txid")},
		{name: "bad proof", err: arkdRefusal(t, "INVALID_INTENT_PROOF"), refused: true},
		{name: "registered coin", err: arkdRefusal(t, "VTXO_ALREADY_REGISTERED"), coinState: true},
		{name: "spent coin", err: arkdRefusal(t, "VTXO_ALREADY_SPENT"), coinState: true},
		{name: "unrolled coin", err: arkdRefusal(t, "VTXO_ALREADY_UNROLLED"), coinState: true},
		{name: "swept coin", err: arkdRefusal(t, "VTXO_RECOVERABLE"), coinState: true},
		{name: "banned coin", err: arkdRefusal(t, "VTXO_BANNED"), coinState: true},
		{name: "unknown coin", err: arkdRefusal(t, "VTXO_NOT_FOUND"), coinState: true},
		{name: "amount over the limit", err: arkdRefusal(t, "AMOUNT_TOO_HIGH"), coinState: true},
		{name: "amount under the limit", err: arkdRefusal(t, "AMOUNT_TOO_LOW"), coinState: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			got := refusal(ctx, tc.err)
			require.Equal(t, tc.refused, chargesTemplate(got))
			require.ErrorIs(t, got, tc.err)
			require.EqualError(t, got, tc.err.Error())
			if !tc.refused && !tc.coinState {
				require.Same(t, tc.err, got)
			}
		})
	}
}

func arkdRefusal(t *testing.T, name string) error {
	t.Helper()
	st, err := status.New(codes.InvalidArgument, name).WithDetails(&arkv1.ErrorDetails{Name: name})
	require.NoError(t, err)
	return st.Err()
}
