package opnode

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum-optimism/optimism/op-node/node"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

type FnTracer struct {
	OnNewL1HeadFn        func(ctx context.Context, sig eth.L1BlockRef)
	OnUnsafeL2PayloadFn  func(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope)
	OnUnsafeL2ResponseFn func(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope)
	OnUnsafeL2RequestFn  func(ctx context.Context, from peer.ID, hash common.Hash)
	OnPublishL2PayloadFn func(ctx context.Context, payload *eth.ExecutionPayloadEnvelope)
}

func (n *FnTracer) OnNewL1Head(ctx context.Context, sig eth.L1BlockRef) {
	if n.OnNewL1HeadFn != nil {
		n.OnNewL1HeadFn(ctx, sig)
	}
}

func (n *FnTracer) OnUnsafeL2Payload(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) {
	if n.OnUnsafeL2PayloadFn != nil {
		n.OnUnsafeL2PayloadFn(ctx, from, payload)
	}
}

// CHANGE(taiko): add OnUnsafeL2Request handler
func (n *FnTracer) OnUnsafeL2Request(ctx context.Context, from peer.ID, hash common.Hash) {
	if n.OnUnsafeL2RequestFn != nil {
		n.OnUnsafeL2RequestFn(ctx, from, hash)
	}
}

// CHANGE(taiko): add OnUnsafeL2Response handler
func (n *FnTracer) OnUnsafeL2Response(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) {
	if n.OnUnsafeL2ResponseFn != nil {
		n.OnUnsafeL2ResponseFn(ctx, from, payload)
	}
}

func (n *FnTracer) OnPublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope) {
	if n.OnPublishL2PayloadFn != nil {
		n.OnPublishL2PayloadFn(ctx, payload)
	}
}

var _ node.Tracer = (*FnTracer)(nil)
