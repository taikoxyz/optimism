package node

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// Tracer configures the OpNode to share events
type Tracer interface {
	OnNewL1Head(ctx context.Context, sig eth.L1BlockRef)
	OnUnsafeL2Payload(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope)
	OnUnsafeL2Request(ctx context.Context, from peer.ID, hash common.Hash)                       // CHANGE(taiko): add OnUnsafeL2Request handler
	OnUnsafeL2Response(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) // CHANGE(taiko): add OnUnsafeL2Response handler
	OnUnsafeL2EndOfSequencingRequest(ctx context.Context, from peer.ID, epoch uint64)            // CHANGE(taiko): add OnUnsafeL2EndOfSequencingRequest handler
	OnPublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope)
}

type noOpTracer struct{}

func (n noOpTracer) OnNewL1Head(ctx context.Context, sig eth.L1BlockRef) {}

func (n noOpTracer) OnUnsafeL2Payload(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) {
}

// CHANGE(taiko): add OnUnsafeL2Request handler
func (n noOpTracer) OnUnsafeL2Request(ctx context.Context, from peer.ID, hash common.Hash) {}

// CHANGE(taiko): add OnUnsafeL2Response handler
func (n noOpTracer) OnUnsafeL2Response(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) {
}

// CHANGE(taiko); add OnUnsafeL2EndOfSequencingRequest handler
func (n noOpTracer) OnUnsafeL2EndOfSequencingRequest(ctx context.Context, from peer.ID, epoch uint64) {
}

func (n noOpTracer) OnPublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope) {}

var _ Tracer = (*noOpTracer)(nil)
