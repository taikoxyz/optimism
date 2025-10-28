package p2p

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/golang/snappy"
	lru "github.com/hashicorp/golang-lru/v2"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

const (
	window       = 45 * time.Second // CHANGE(taiko): window for accepting the same hash
	refillPerMin = 200              // CHANGE(taiko): tokens/min per peer
	maxTokens    = refillPerMin     // CHANGE(taiko): max tokens per peer
	// maxGossipSize limits the total size of gossip RPC containers as well as decompressed individual messages.
	maxGossipSize = 10 * (1 << 20)
	// minGossipSize is used to make sure that there is at least some data to validate the signature against.
	minGossipSize          = 66
	maxOutboundQueue       = 768 // CHANGE(taiko): upgrade queue size
	maxValidateQueue       = 768 // CHANGE(taiko): upgrade queue size
	globalValidateThrottle = 512
	gossipHeartbeat        = 500 * time.Millisecond
	// seenMessagesTTL limits the duration that message IDs are remembered for gossip deduplication purposes
	// 130 * gossipHeartbeat
	seenMessagesTTL  = 130 * gossipHeartbeat
	DefaultMeshD     = 8  // topic stable mesh target count
	DefaultMeshDlo   = 6  // topic stable mesh low watermark
	DefaultMeshDhi   = 12 // topic stable mesh high watermark
	DefaultMeshDlazy = 6  // gossip target
	// peerScoreInspectFrequency is the frequency at which peer scores are inspected
	peerScoreInspectFrequency = 15 * time.Second
	defaultBufferSize         = 768  // CHANGE(taiko): change sizes to contants
	defaultLRUCacheSize       = 1000 // CHANGE(taiko): change sizes to contants
	expectedSigLen            = 65   // CHANGE(taiko): expected signature length for the sequencer signature
	maxResponsesAcceptable    = 3    // CHANGE(taiko): max responses acceptable for preconf blocks
)

// Message domains, the msg id function uncompresses to keep data monomorphic,
// but invalid compressed data will need a unique different id.

var MessageDomainInvalidSnappy = [4]byte{0, 0, 0, 0}
var MessageDomainValidSnappy = [4]byte{1, 0, 0, 0}

type GossipSetupConfigurables interface {
	PeerScoringParams() *ScoringParams
	// ConfigureGossip creates configuration options to apply to the GossipSub setup
	ConfigureGossip(rollupCfg *rollup.Config) []pubsub.Option
}

type GossipRuntimeConfig interface {
	P2PSequencerAddress() common.Address
}

type PreconfGossipRuntimeConfig interface {
	P2PSequencerAddresses() []common.Address // CHANGE(taiko): new impl of preconf gossip runtime config
}

// CHANGE(taiko): optional provider for lookahead schedule expectations used to validate preconfirmations
type PreconfScheduleRuntime interface {
	// Current committer that should sign the commitment
	CurrentPreconferCommitter() common.Address
}

//go:generate mockery --name GossipMetricer
type GossipMetricer interface {
	RecordGossipEvent(evType int32)
}

func blocksTopicV1(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/0/blocks", cfg.L2ChainID.String())
}

func blocksTopicV2(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/1/blocks", cfg.L2ChainID.String())
}

func blocksTopicV3(cfg *rollup.Config) string {
	return fmt.Sprintf("/optimism/%s/2/blocks", cfg.L2ChainID.String())
}

// CHANGE(taiko): create preconf blocks topic.
func preconfBlocksTopicV1(cfg *rollup.Config) string {
	return fmt.Sprintf("/taiko/%s/0/preconfBlocks", cfg.L2ChainID.String())
}

// CHANGE(taiko): create preconf blocks request topic.
func preconfBlocksRequestTopic(cfg *rollup.Config) string {
	return fmt.Sprintf("/taiko/%s/0/requestPreconfBlocks", cfg.L2ChainID.String())
}

// CHANGE(taiko): create preconf blocks end of sequencing request topic.
func preconfBlocksEndOfSequencingRequestTopic(cfg *rollup.Config) string {
	return fmt.Sprintf("/taiko/%s/0/requestEndOfSequencingPreconfBlocks", cfg.L2ChainID.String())
}

// CHANGE(taiko): create preconf blocks response topic.
func preconfBlocksResponseTopic(cfg *rollup.Config) string {
	return fmt.Sprintf("/taiko/%s/0/responsePreconfBlocks", cfg.L2ChainID.String())
}

// CHANGE(taiko): Carries SignedCommitment messages on the v2 response path for compatibility.
func preconfirmationsCommitmentTopic(cfg *rollup.Config) string {
	return fmt.Sprintf("/taiko/%s/0/preconfirmationCommitments", cfg.L2ChainID.String())
}

// BuildSubscriptionFilter builds a simple subscription filter,
// to help protect against peers spamming useless subscriptions.
func BuildSubscriptionFilter(cfg *rollup.Config) pubsub.SubscriptionFilter {
	return pubsub.NewAllowlistSubscriptionFilter(
		blocksTopicV1(cfg),
		blocksTopicV2(cfg),
		blocksTopicV3(cfg),
		preconfBlocksTopicV1(cfg),
		preconfBlocksRequestTopic(cfg),
		preconfBlocksResponseTopic(cfg),
		preconfirmationsCommitmentTopic(cfg),
		preconfBlocksEndOfSequencingRequestTopic(cfg),
	) // add more topics here in the future, if any.
}

var msgBufPool = sync.Pool{New: func() any {
	// note: the topic validator concurrency is limited, so pool won't blow up, even with large pre-allocation.
	x := make([]byte, 0, maxGossipSize)
	return &x
}}

// BuildMsgIdFn builds a generic message ID function for gossipsub that can handle compressed payloads,
// mirroring the eth2 p2p gossip spec.
func BuildMsgIdFn(cfg *rollup.Config) pubsub.MsgIdFunction {
	return func(pmsg *pb.Message) string {
		valid := false
		var data []byte
		// If it's a valid compressed snappy data, then hash the uncompressed contents.
		// The validator can throw away the message later when recognized as invalid,
		// and the unique hash helps detect duplicates.
		dLen, err := snappy.DecodedLen(pmsg.Data)
		if err == nil && dLen <= maxGossipSize {
			res := msgBufPool.Get().(*[]byte)
			defer msgBufPool.Put(res)
			if data, err = snappy.Decode((*res)[:cap(*res)], pmsg.Data); err == nil {
				if cap(data) > cap(*res) {
					// if we ended up growing the slice capacity, fine, keep the larger one.
					*res = data[:cap(data)]
				}
				valid = true
			}
		}
		if data == nil {
			data = pmsg.Data
		}
		h := sha256.New()
		if valid {
			h.Write(MessageDomainValidSnappy[:])
		} else {
			h.Write(MessageDomainInvalidSnappy[:])
		}
		// The chain ID is part of the gossip topic, making the msg id unique
		topic := pmsg.GetTopic()
		var topicLen [8]byte
		binary.LittleEndian.PutUint64(topicLen[:], uint64(len(topic)))
		h.Write(topicLen[:])
		h.Write([]byte(topic))
		h.Write(data)
		// the message ID is shortened to save space, a lot of these may be gossiped.
		return string(h.Sum(nil)[:20])
	}
}

func (p *Config) ConfigureGossip(rollupCfg *rollup.Config) []pubsub.Option {
	params := BuildGlobalGossipParams(rollupCfg)

	// override with CLI changes
	params.D = p.MeshD
	params.Dlo = p.MeshDLo
	params.Dhi = p.MeshDHi
	params.Dlazy = p.MeshDLazy

	// in the future we may add more advanced options like scoring and PX / direct-mesh / episub
	return []pubsub.Option{
		pubsub.WithGossipSubParams(params),
		pubsub.WithFloodPublish(p.FloodPublish),
	}
}

func BuildGlobalGossipParams(cfg *rollup.Config) pubsub.GossipSubParams {
	params := pubsub.DefaultGossipSubParams()
	params.D = DefaultMeshD                    // topic stable mesh target count
	params.Dlo = DefaultMeshDlo                // topic stable mesh low watermark
	params.Dhi = DefaultMeshDhi                // topic stable mesh high watermark
	params.Dlazy = DefaultMeshDlazy            // gossip target
	params.HeartbeatInterval = gossipHeartbeat // interval of heartbeat
	params.FanoutTTL = 24 * time.Second        // ttl for fanout maps for topics we are not subscribed to but have published to
	params.HistoryLength = 12                  // number of windows to retain full messages in cache for IWANT responses
	params.HistoryGossip = 3                   // number of windows to gossip about

	return params
}

// NewGossipSub configures a new pubsub instance with the specified parameters.
// PubSub uses a GossipSubRouter as it's router under the hood.
func NewGossipSub(p2pCtx context.Context, h host.Host, cfg *rollup.Config, gossipConf GossipSetupConfigurables, scorer Scorer, m GossipMetricer, log log.Logger) (*pubsub.PubSub, error) {
	denyList, err := pubsub.NewTimeCachedBlacklist(30 * time.Second)
	if err != nil {
		return nil, err
	}
	gossipOpts := []pubsub.Option{
		pubsub.WithMaxMessageSize(maxGossipSize),
		pubsub.WithMessageIdFn(BuildMsgIdFn(cfg)),
		pubsub.WithNoAuthor(),
		pubsub.WithMessageSignaturePolicy(pubsub.StrictNoSign),
		pubsub.WithSubscriptionFilter(BuildSubscriptionFilter(cfg)),
		pubsub.WithValidateQueueSize(maxValidateQueue),
		pubsub.WithPeerOutboundQueueSize(maxOutboundQueue),
		pubsub.WithValidateThrottle(globalValidateThrottle),
		pubsub.WithSeenMessagesTTL(seenMessagesTTL),
		pubsub.WithPeerExchange(false),
		pubsub.WithBlacklist(denyList),
		pubsub.WithEventTracer(&gossipTracer{m: m}),
	}
	gossipOpts = append(gossipOpts, ConfigurePeerScoring(gossipConf, scorer, log)...)
	gossipOpts = append(gossipOpts, gossipConf.ConfigureGossip(cfg)...)
	return pubsub.NewGossipSub(p2pCtx, h, gossipOpts...)
}

func validationResultString(v pubsub.ValidationResult) string {
	switch v {
	case pubsub.ValidationAccept:
		return "ACCEPT"
	case pubsub.ValidationIgnore:
		return "IGNORE"
	case pubsub.ValidationReject:
		return "REJECT"
	default:
		return fmt.Sprintf("UNKNOWN_%d", v)
	}
}

func logValidationResult(self peer.ID, msg string, log log.Logger, fn pubsub.ValidatorEx) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		res := fn(ctx, id, message)
		var src any
		src = id
		if id == self {
			src = "self"
		}
		log.Debug(msg, "result", validationResultString(res), "from", src)
		return res
	}
}

func guardGossipValidator(log log.Logger, fn pubsub.ValidatorEx) pubsub.ValidatorEx {
	return func(ctx context.Context, id peer.ID, message *pubsub.Message) (result pubsub.ValidationResult) {
		defer func() {
			if err := recover(); err != nil {
				log.Error("gossip validation panic", "err", err, "peer", id)
				result = pubsub.ValidationReject
			}
		}()
		return fn(ctx, id, message)
	}
}

type seenBlocks struct {
	sync.Mutex
	blockHashes []common.Hash
}

// hasSeen checks if the hash has been marked as seen, and how many have been seen.
func (sb *seenBlocks) hasSeen(h common.Hash) (count int, hasSeen bool) {
	sb.Lock()
	defer sb.Unlock()
	for _, prev := range sb.blockHashes {
		if prev == h {
			return len(sb.blockHashes), true
		}
	}
	return len(sb.blockHashes), false
}

// markSeen marks the block hash as seen
func (sb *seenBlocks) markSeen(h common.Hash) {
	sb.Lock()
	defer sb.Unlock()
	sb.blockHashes = append(sb.blockHashes, h)
}

// CHANGE(taiko): add seenEpochs cache.
type seenEpochs struct {
	sync.Mutex
	epochs map[uint64]uint64
}

// CHANGE(taiko): numSeen checks if the epoch has been marked as seen, and how many have been seen.
func (se *seenEpochs) numSeen(epoch uint64) (count uint64, hasSeen bool) {
	se.Lock()
	defer se.Unlock()
	count, hasSeen = se.epochs[epoch]
	return
}

// CHANGE(taiko): markSeen marks epoch as seen
func (se *seenEpochs) markSeen(epoch uint64) {
	se.Lock()
	defer se.Unlock()
	if _, ok := se.epochs[epoch]; !ok {
		se.epochs[epoch] = 1
	} else {
		se.epochs[epoch]++
	}
}

// CHANGE(taiko): add preconfBlocks topic validator
func BuildPreconfBlocksValidator(
	logger log.Logger,
	cfg *rollup.Config,
	runCfg GossipRuntimeConfig,
	blockVersion eth.BlockVersion,
) pubsub.ValidatorEx {
	// Seen block hashes per block height
	preconfLRU, err := lru.New[uint64, *seenBlocks](defaultLRUCacheSize)
	if err != nil {
		panic(fmt.Errorf("failed to set up block height LRU cache: %w", err))
	}

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		// 1) Snappy length sanity checks
		outLen, err := snappy.DecodedLen(message.Data)
		if err != nil {
			log.Warn("invalid snappy compression length data", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen > maxGossipSize {
			log.Warn("possible snappy zip bomb", "decoded_length", outLen, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen < minGossipSize {
			log.Warn("undersized gossip payload", "peer", id)
			return pubsub.ValidationReject
		}

		// 2) Snappy-decode into pooled buffer
		bufPtr := msgBufPool.Get().(*[]byte)
		defer msgBufPool.Put(bufPtr)
		data, err := snappy.Decode((*bufPtr)[:cap(*bufPtr)], message.Data)
		if err != nil {
			logger.Warn("snappy decode failed", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if cap(data) > cap(*bufPtr) {
			*bufPtr = data[:cap(data)]
		}

		// 3) Split off the wire signature prefix
		if len(data) < expectedSigLen {
			logger.Warn("payload too short to contain wire signature", "peer", id)
			return pubsub.ValidationReject
		}

		signatureBytes, payloadBytes := data[:expectedSigLen], data[expectedSigLen:]

		// 4) Verify the sequencer’s wire signature
		if res := verifyBlockSignature(logger, cfg, runCfg, id, signatureBytes, payloadBytes); res != pubsub.ValidationAccept {
			return res
		}

		// 5) Now SSZ-decode the payloadBytes into the envelope
		var envelope eth.ExecutionPayloadEnvelope
		if err := envelope.UnmarshalSSZ(uint32(len(payloadBytes)), bytes.NewReader(payloadBytes)); err != nil {
			logger.Warn("invalid envelope payload", "err", err, "peer", id)
			return pubsub.ValidationReject
		}

		// 6) Sanity-check the inner ExecutionPayload
		payload := envelope.ExecutionPayload
		if payload == nil {
			logger.Warn("payload is empty", "peer", id)
			return pubsub.ValidationReject
		}
		if len(payload.Transactions) == 0 {
			logger.Warn("payload has empty transaction data", "peer", id)
			return pubsub.ValidationReject
		}
		if payload.FeeRecipient == (common.Address{}) {
			logger.Warn("empty coinbase in payload", "peer", id)
			return pubsub.ValidationReject
		}
		if payload.BlockNumber == 0 {
			logger.Warn("payload has zero block ID", "peer", id)
			return pubsub.ValidationReject
		}

		// 7) Deduplicate by block number + hash
		height := uint64(payload.BlockNumber)
		seen, ok := preconfLRU.Get(height)
		if !ok {
			seen = new(seenBlocks)
			preconfLRU.Add(height, seen)
		}
		if count, _ := seen.hasSeen(payload.BlockHash); count > 10 {
			logger.Warn("seen same block hash too many times in preconf response", "height", payload.BlockNumber)
			return pubsub.ValidationIgnore
		}
		seen.markSeen(payload.BlockHash)

		// 8) All good—stash the decoded envelope for the subscriber
		message.ValidatorData = &envelope
		return pubsub.ValidationAccept
	}
}

// CHANGE(taiko): add preconfBlocks topic validator
func BuildPreconfBlocksResponseValidator(
	log log.Logger,
	cfg *rollup.Config,
	runCfg GossipRuntimeConfig,
	blockVersion eth.BlockVersion,
) pubsub.ValidatorEx {
	// Seen block hashes per block height
	preconfblockLRU, err := lru.New[uint64, *seenBlocks](defaultLRUCacheSize)
	if err != nil {
		panic(fmt.Errorf("failed to set up block height LRU cache: %w", err))
	}

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		// 1) Snappy‐length sanity checks
		outLen, err := snappy.DecodedLen(message.Data)
		if err != nil {
			log.Warn("invalid snappy compression length data", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen > maxGossipSize {
			log.Warn("possible snappy zip bomb", "decoded_length", outLen, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen < minGossipSize {
			log.Warn("rejecting undersized gossip payload")
			return pubsub.ValidationReject
		}

		// 2) Snappy‐decode into pooled buffer
		bufPtr := msgBufPool.Get().(*[]byte)
		defer msgBufPool.Put(bufPtr)
		data, err := snappy.Decode((*bufPtr)[:cap(*bufPtr)], message.Data)
		if err != nil {
			log.Warn("invalid snappy compression", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if cap(data) > cap(*bufPtr) {
			*bufPtr = data[:cap(data)]
		}

		var envelope eth.ExecutionPayloadEnvelope
		if err := envelope.UnmarshalSSZ(uint32(len(data)), bytes.NewReader(data)); err != nil {
			log.Warn("invalid envelope payload", "err", err, "peer", id)
			return pubsub.ValidationReject
		}

		// 5) must have that embedded 65B signature
		if envelope.Signature == nil {
			log.Warn("missing envelope signature", "peer", id)
			return pubsub.ValidationReject
		}

		if res := verifyBlockResponseSignature(log, cfg, runCfg, id, envelope.Signature[:], envelope.ExecutionPayload.BlockHash.Bytes()); res == pubsub.ValidationReject {
			return res
		}

		// 7) Payload sanity checks
		payload := envelope.ExecutionPayload
		if payload == nil {
			log.Warn("payload is empty", "peer", id)
			return pubsub.ValidationReject
		}
		if len(payload.Transactions) == 0 {
			log.Warn("payload has empty transaction data", "peer", id)
			return pubsub.ValidationReject
		}
		if payload.FeeRecipient == (common.Address{}) {
			log.Warn("empty coinbase in payload", "peer", id)
			return pubsub.ValidationReject
		}
		if payload.BlockNumber == 0 {
			log.Warn("payload has zero block ID", "peer", id)
			return pubsub.ValidationReject
		}

		// 8) Deduplicate by block number + hash
		height := uint64(payload.BlockNumber)
		seen, ok := preconfblockLRU.Get(height)
		if !ok {
			seen = new(seenBlocks)
			preconfblockLRU.Add(height, seen)
		}
		if count, _ := seen.hasSeen(payload.BlockHash); count > maxResponsesAcceptable {
			log.Warn("seen same block hash too many times in preconf response", "height", payload.BlockNumber)
			return pubsub.ValidationIgnore
		}
		seen.markSeen(payload.BlockHash)

		// 9) All good—stash the decoded envelope for later usage
		message.ValidatorData = &envelope
		return pubsub.ValidationAccept
	}
}

// CHANGE(taiko): add preconfBlocksRequest topic validator
func BuildPreconfBlocksRequestValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig) pubsub.ValidatorEx {
	buckets, err := lru.New[peer.ID, *rateBucket](defaultLRUCacheSize) // per‑peer token buckets
	if err != nil {
		panic(fmt.Errorf("per‑peer buckets: %w", err))
	}
	seenHash, err := lru.New[common.Hash, time.Time](defaultLRUCacheSize) // per‑hash window
	if err != nil {
		panic(fmt.Errorf("seen‑hash LRU: %w", err))
	}

	var bucketsMu sync.Mutex
	rate := float64(refillPerMin) / 60.0 // tokens per second

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		now := time.Now()
		hash := common.BytesToHash(message.Data)

		// Per-hash time window: drop repeats for a short window.
		if t, ok := seenHash.Get(hash); ok && now.Sub(t) < window {
			return pubsub.ValidationIgnore
		}

		// Per-peer token bucket
		bucketsMu.Lock()
		defer bucketsMu.Unlock()

		cap := float64(maxTokens)

		// per message (inside bucketsMu.Lock()):
		b, ok := buckets.Get(id)
		if !ok {
			b = &rateBucket{credit: cap, last: now}
			buckets.Add(id, b)
		} else {
			refillBucket(b, now, rate, cap)
		}
		if !consumeToken(b, 1.0) {
			return pubsub.ValidationIgnore
		}

		// Mark seen after passing rate limit.
		seenHash.Add(hash, now)
		message.ValidatorData = hash
		return pubsub.ValidationAccept
	}
}

// CHANGE(taiko): validator for preconfirmations (SignedCommitment)
func BuildPreconfirmationValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig) pubsub.ValidatorEx {
	buckets, err := lru.New[peer.ID, *rateBucket](defaultLRUCacheSize) // per‑peer token buckets
	if err != nil {
		panic(fmt.Errorf("per‑peer buckets: %w", err))
	}
	seenMsg, err := lru.New[common.Hash, time.Time](defaultLRUCacheSize)
	if err != nil {
		panic(fmt.Errorf("seen‑msg LRU: %w", err))
	}

	var bucketsMu sync.Mutex
	rate := float64(refillPerMin) / 60.0 // tokens per second

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		now := time.Now()
		msgHash := crypto.Keccak256Hash(message.Data)
		if t, ok := seenMsg.Get(msgHash); ok && now.Sub(t) < window {
			return pubsub.ValidationIgnore
		}

		// Per-peer token bucket
		bucketsMu.Lock()
		defer bucketsMu.Unlock()
		cap := float64(maxTokens)
		b, ok := buckets.Get(id)
		if !ok {
			b = &rateBucket{credit: cap, last: now}
			buckets.Add(id, b)
		} else {
			refillBucket(b, now, rate, cap)
		}
		if !consumeToken(b, 1.0) {
			return pubsub.ValidationIgnore
		}

		// Decode SSZ SignedCommitment
		var sc SignedCommitment
		if err := sc.UnmarshalSSZ(uint32(len(message.Data)), bytes.NewReader(message.Data)); err != nil {
			log.Warn("invalid preconfirmation SSZ", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if len(sc.Signature) != expectedSigLen {
			return pubsub.ValidationReject
		}

		// Verify signature and authorize (schedule/whitelist)
		if res := verifyPreconfirmationSignature(log, cfg, runCfg, id, &sc); res != pubsub.ValidationAccept {
			return res
		}

		// Business-logic validation of the commitment payload
		pc := sc.Commitment.Preconf
		// Basic nil checks for big.Int fields
		if pc.BlockNumber == nil || pc.AnchorBlockNumber == nil || pc.ParentSubmissionWindowEnd == nil || pc.SubmissionWindowEnd == nil {
			log.Warn("preconf has nil numeric fields", "peer", id)
			return pubsub.ValidationReject
		}
		// Non-negative / sensible ordering
		if pc.BlockNumber.Sign() <= 0 {
			log.Warn("preconf has non-positive block number", "peer", id)
			return pubsub.ValidationReject
		}
		if pc.AnchorBlockNumber.Sign() < 0 {
			log.Warn("preconf has negative anchor block number", "peer", id)
			return pubsub.ValidationReject
		}
		if pc.BlockNumber.Cmp(pc.AnchorBlockNumber) < 0 {
			log.Warn("preconf anchor exceeds block number", "peer", id, "block", pc.BlockNumber, "anchor", pc.AnchorBlockNumber)
			return pubsub.ValidationReject
		}
		if pc.SubmissionWindowEnd.Cmp(pc.ParentSubmissionWindowEnd) < 0 {
			log.Warn("preconf submission window regress", "peer", id)
			return pubsub.ValidationReject
		}
		// Hash presence
		if pc.ParentRawTxListHash == (common.Hash{}) || pc.RawTxListHash == (common.Hash{}) {
			log.Warn("preconf missing tx list hashes", "peer", id)
			return pubsub.ValidationReject
		}

		seenMsg.Add(msgHash, now)
		message.ValidatorData = &sc
		return pubsub.ValidationAccept
	}
}

// CHANGE(taiko): add preconfBlocksRequest topic validator
func BuildPreconfBlocksEndOfSequencingRequestValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig) pubsub.ValidatorEx {
	buckets, err := lru.New[peer.ID, *rateBucket](defaultLRUCacheSize) // per‑peer token buckets
	if err != nil {
		panic(fmt.Errorf("per‑peer buckets: %w", err))
	}
	// Seen block hashes per block height
	// uint64 -> *seenBlocks
	epochLRU, err := lru.New[uint64, *seenEpochs](defaultLRUCacheSize)
	if err != nil {
		panic(fmt.Errorf("failed to set up epoch LRU cache: %w", err))
	}

	var bucketsMu sync.Mutex
	rate := float64(refillPerMin) / 60.0 // tokens per second

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		now := time.Now()
		epoch := big.NewInt(0).SetBytes(message.Data).Uint64()

		// Per-epoch duplicate limiter (cap total seen responses per epoch).
		seen, ok := epochLRU.Get(epoch)
		if !ok {
			seen = new(seenEpochs)
			seen.epochs = make(map[uint64]uint64)
			epochLRU.Add(epoch, seen)
		}
		if count, hasSeen := seen.numSeen(epoch); hasSeen && count > maxResponsesAcceptable {
			return pubsub.ValidationIgnore
		}

		// Per-peer token bucket
		bucketsMu.Lock()
		defer bucketsMu.Unlock()
		cap := float64(maxTokens)

		// per message (inside bucketsMu.Lock()):
		b, ok := buckets.Get(id)
		if !ok {
			b = &rateBucket{credit: cap, last: now}
			buckets.Add(id, b)
		} else {
			refillBucket(b, now, rate, cap)
		}
		if !consumeToken(b, 1.0) {
			return pubsub.ValidationIgnore
		}

		// Count only after rate‑limit passes.
		seen.markSeen(epoch)

		message.ValidatorData = epoch
		return pubsub.ValidationAccept
	}
}

func BuildBlocksValidator(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, blockVersion eth.BlockVersion) pubsub.ValidatorEx {
	// Seen block hashes per block height
	// uint64 -> *seenBlocks
	blockHeightLRU, err := lru.New[uint64, *seenBlocks](defaultLRUCacheSize) // CHANGE(taiko): change sizes to contants
	if err != nil {
		panic(fmt.Errorf("failed to set up block height LRU cache: %w", err))
	}

	return func(ctx context.Context, id peer.ID, message *pubsub.Message) pubsub.ValidationResult {
		// [REJECT] if the compression is not valid
		outLen, err := snappy.DecodedLen(message.Data)
		if err != nil {
			log.Warn("invalid snappy compression length data", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen > maxGossipSize {
			log.Warn("possible snappy zip bomb, decoded length is too large", "decoded_length", outLen, "peer", id)
			return pubsub.ValidationReject
		}
		if outLen < minGossipSize {
			log.Warn("rejecting undersized gossip payload")
			return pubsub.ValidationReject
		}

		res := msgBufPool.Get().(*[]byte)
		defer msgBufPool.Put(res)
		data, err := snappy.Decode((*res)[:cap(*res)], message.Data)
		if err != nil {
			log.Warn("invalid snappy compression", "err", err, "peer", id)
			return pubsub.ValidationReject
		}
		// if we ended up growing the slice capacity, fine, keep the larger one.
		if cap(data) > cap(*res) {
			*res = data[:cap(data)]
		}

		// message starts with compact-encoding secp256k1 encoded signature
		signatureBytes, payloadBytes := data[:65], data[65:]

		// [REJECT] if the signature by the sequencer is not valid
		result := verifyBlockSignature(log, cfg, runCfg, id, signatureBytes, payloadBytes)
		if result != pubsub.ValidationAccept {
			return result
		}

		var envelope eth.ExecutionPayloadEnvelope

		// [REJECT] if the block encoding is not valid
		if blockVersion == eth.BlockV3 {
			if err := envelope.UnmarshalSSZ(uint32(len(payloadBytes)), bytes.NewReader(payloadBytes)); err != nil {
				log.Warn("invalid envelope payload", "err", err, "peer", id)
				return pubsub.ValidationReject
			}
		} else {
			var payload eth.ExecutionPayload
			if err := payload.UnmarshalSSZ(blockVersion, uint32(len(payloadBytes)), bytes.NewReader(payloadBytes)); err != nil {
				log.Warn("invalid execution payload", "err", err, "peer", id)
				return pubsub.ValidationReject
			}
			envelope = eth.ExecutionPayloadEnvelope{ExecutionPayload: &payload}
		}

		payload := envelope.ExecutionPayload

		// rounding down to seconds is fine here.
		now := uint64(time.Now().Unix())

		// [REJECT] if the `payload.timestamp` is older than 60 seconds in the past
		if uint64(payload.Timestamp) < now-60 {
			log.Warn("payload is too old", "timestamp", uint64(payload.Timestamp))
			return pubsub.ValidationReject
		}

		// [REJECT] if the `payload.timestamp` is more than 5 seconds into the future
		if uint64(payload.Timestamp) > now+5 {
			log.Warn("payload is too new", "timestamp", uint64(payload.Timestamp))
			return pubsub.ValidationReject
		}

		// [REJECT] if the `block_hash` in the `payload` is not valid
		if actual, ok := envelope.CheckBlockHash(); !ok {
			log.Warn("payload has bad block hash", "bad_hash", payload.BlockHash.String(), "actual", actual.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a V1 Block has withdrawals
		if !blockVersion.HasWithdrawals() && payload.Withdrawals != nil {
			log.Warn("payload is on v1 topic, but has withdrawals", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a >= V2 Block does not have withdrawals
		if blockVersion.HasWithdrawals() && payload.Withdrawals == nil {
			log.Warn("payload is on v2/v3 topic, but does not have withdrawals", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if a >= V2 Block has non-empty withdrawals
		if blockVersion.HasWithdrawals() && len(*payload.Withdrawals) != 0 {
			log.Warn("payload is on v2/v3 topic, but has non-empty withdrawals", "bad_hash", payload.BlockHash.String(), "withdrawal_count", len(*payload.Withdrawals))
			return pubsub.ValidationReject
		}

		// [REJECT] if the block is on a topic <= V2 and has a blob gas value set
		if !blockVersion.HasBlobProperties() && payload.BlobGasUsed != nil {
			log.Warn("payload is on v1/v2 topic, but has blob gas used", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		// [REJECT] if the block is on a topic <= V2 and has an excess blob gas value set
		if !blockVersion.HasBlobProperties() && payload.ExcessBlobGas != nil {
			log.Warn("payload is on v1/v2 topic, but has excess blob gas", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		if blockVersion.HasBlobProperties() {
			// [REJECT] if the block is on a topic >= V3 and has a blob gas used value that is not zero
			if payload.BlobGasUsed == nil || *payload.BlobGasUsed != 0 {
				log.Warn("payload is on v3 topic, but has non-zero blob gas used", "bad_hash", payload.BlockHash.String(), "blob_gas_used", payload.BlobGasUsed)
				return pubsub.ValidationReject
			}

			// [REJECT] if the block is on a topic >= V3 and has an excess blob gas value that is not zero
			if payload.ExcessBlobGas == nil || *payload.ExcessBlobGas != 0 {
				log.Warn("payload is on v3 topic, but has non-zero excess blob gas", "bad_hash", payload.BlockHash.String(), "excess_blob_gas", payload.ExcessBlobGas)
				return pubsub.ValidationReject
			}
		}

		// [REJECT] if the block is on a topic >= V3 and the parent beacon block root is nil
		if blockVersion.HasParentBeaconBlockRoot() && envelope.ParentBeaconBlockRoot == nil {
			log.Warn("payload is on v3 topic, but has nil parent beacon block root", "bad_hash", payload.BlockHash.String())
			return pubsub.ValidationReject
		}

		seen, ok := blockHeightLRU.Get(uint64(payload.BlockNumber))
		if !ok {
			seen = new(seenBlocks)
			blockHeightLRU.Add(uint64(payload.BlockNumber), seen)
		}

		if count, hasSeen := seen.hasSeen(payload.BlockHash); count > 5 {
			// [REJECT] if more than 5 blocks have been seen with the same block height
			log.Warn("seen too many different blocks at same height", "height", payload.BlockNumber)
			return pubsub.ValidationReject
		} else if hasSeen {
			// [IGNORE] if the block has already been seen
			log.Warn("validated already seen message again")
			return pubsub.ValidationIgnore
		}

		// mark it as seen. (note: with concurrent validation more than 5 blocks may be marked as seen still,
		// but validator concurrency is limited anyway)
		seen.markSeen(payload.BlockHash)

		// remember the decoded payload for later usage in topic subscriber.
		message.ValidatorData = &envelope
		return pubsub.ValidationAccept
	}
}

func verifyBlockSignature(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, id peer.ID, signatureBytes []byte, payloadBytes []byte) pubsub.ValidationResult {
	signingHash, err := BlockSigningHash(cfg, payloadBytes)
	if err != nil {
		log.Warn("failed to compute block signing hash", "err", err, "peer", id)
		return pubsub.ValidationReject
	}

	pub, err := crypto.SigToPub(signingHash[:], signatureBytes)
	if err != nil {
		log.Warn("invalid block signature", "err", err, "peer", id)
		return pubsub.ValidationReject
	}
	addr := crypto.PubkeyToAddress(*pub)

	log.Debug("verifying block signature", "peer", id, "addr", addr.Hex(), "signing_hash", signingHash.Hex())

	// CHANGE(taiko): check if the signer is in the whitelist.
	if cfg, ok := runCfg.(PreconfGossipRuntimeConfig); ok {
		// If the signer is in the whitelist, accept the block.
		if slices.Contains(cfg.P2PSequencerAddresses(), addr) {
			return pubsub.ValidationAccept
		}
		log.Warn("unexpected block authors", "peer", id, "addrs", cfg.P2PSequencerAddresses())

		return pubsub.ValidationReject
	}

	// In the future we may load & validate block metadata before checking the signature.
	// And then check the signer based on the metadata, to support e.g. multiple p2p signers at the same time.
	// For now we only have one signer at a time and thus check the address directly.
	// This means we may drop old payloads upon key rotation,
	// but this can be recovered from like any other missed unsafe payload.
	if expected := runCfg.P2PSequencerAddress(); expected == (common.Address{}) {
		log.Warn("no configured p2p sequencer address, ignoring gossiped block", "peer", id, "addr", addr)
		return pubsub.ValidationIgnore
	} else if addr != expected {
		log.Warn("unexpected block author", "err", err, "peer", id, "addr", addr, "expected", expected)
		return pubsub.ValidationReject
	}

	return pubsub.ValidationAccept
}

// CHANGE(taiko): verifies the block response signature
func verifyBlockResponseSignature(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, id peer.ID, signatureBytes []byte, payloadBytes []byte) pubsub.ValidationResult {
	signingHash, err := BlockSigningHash(cfg, payloadBytes)
	if err != nil {
		log.Warn("failed to compute block signing hash", "err", err, "peer", id)
		return pubsub.ValidationReject
	}

	pub, err := crypto.SigToPub(signingHash[:], signatureBytes)
	if err != nil {
		log.Warn("invalid block response signature", "err", err, "peer", id)
		return pubsub.ValidationReject
	}
	addr := crypto.PubkeyToAddress(*pub)

	// CHANGE(taiko): check if the signer is in the whitelist.
	if cfg, ok := runCfg.(PreconfGossipRuntimeConfig); ok {
		if len(cfg.P2PSequencerAddresses()) == 0 {
			return pubsub.ValidationIgnore
		}
		// If the signer is in the whitelist, accept the block.
		if slices.Contains(cfg.P2PSequencerAddresses(), addr) {
			return pubsub.ValidationAccept
		}

		log.Warn("unexpected block response authors", "err", err, "peer", id, "addr", cfg.P2PSequencerAddresses())
		return pubsub.ValidationReject
	}

	// In the future we may load & validate block metadata before checking the signature.
	// And then check the signer based on the metadata, to support e.g. multiple p2p signers at the same time.
	// For now we only have one signer at a time and thus check the address directly.
	// This means we may drop old payloads upon key rotation,
	// but this can be recovered from like any other missed unsafe payload.
	if expected := runCfg.P2PSequencerAddress(); expected == (common.Address{}) {
		log.Warn("no configured p2p sequencer address, ignoring gossiped block response", "peer", id, "addr", addr)
		return pubsub.ValidationIgnore
	} else if addr != expected {
		log.Warn("unexpected block response author", "err", err, "peer", id, "addr", addr, "expected", expected)
		return pubsub.ValidationReject
	}
	return pubsub.ValidationAccept
}

// CHANGE(taiko): verifies the preconfirmation commitment signature
func verifyPreconfirmationSignature(log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, id peer.ID, sc *SignedCommitment) pubsub.ValidationResult {
	// Marshal commitment (without signature)
	var buf bytes.Buffer
	if _, err := sc.Commitment.MarshalSSZ(&buf); err != nil {
		log.Warn("failed to encode preconf commitment for signing", "err", err, "peer", id)
		return pubsub.ValidationReject
	}

	signingHash, err := BlockSigningHash(cfg, buf.Bytes())
	if err != nil {
		log.Warn("failed to compute preconf signing hash", "err", err, "peer", id)
		return pubsub.ValidationReject
	}

	pub, err := crypto.SigToPub(signingHash[:], sc.Signature)
	if err != nil {
		log.Warn("invalid preconf signature", "err", err, "peer", id)
		return pubsub.ValidationReject
	}
	addr := crypto.PubkeyToAddress(*pub)

	// If schedule provides a committer, enforce it strictly.
	if sched, ok := runCfg.(PreconfScheduleRuntime); ok {
		if committer := sched.CurrentPreconferCommitter(); committer != (common.Address{}) {
			if addr == committer {
				return pubsub.ValidationAccept
			}
		}
	}
	return pubsub.ValidationReject
}

type GossipIn interface {
	OnUnsafeL2Payload(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error
	OnUnsafeL2Request(ctx context.Context, from peer.ID, msg common.Hash) error
	OnUnsafeL2EndOfSequencingRequest(ctx context.Context, from peer.ID, epoch uint64) error
	OnUnsafeL2Response(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error
	// CHANGE(taiko): new preconfirmation handler
	OnUnsafePreconfirmationCommitment(ctx context.Context, from peer.ID, msg *SignedCommitment) error
}

type GossipTopicInfo interface {
	AllBlockTopicsPeers() []peer.ID
	BlocksTopicV1Peers() []peer.ID
	BlocksTopicV2Peers() []peer.ID
	BlocksTopicV3Peers() []peer.ID
}

type GossipOut interface {
	GossipTopicInfo
	PublishL2Payload(ctx context.Context, msg *eth.ExecutionPayloadEnvelope, signer Signer) error
	PublishL2RequestResponse(ctx context.Context, msg *eth.ExecutionPayloadEnvelope, signer Signer) error
	PublishL2Request(ctx context.Context, hash common.Hash) error            // TODO: add signer, sign request
	PublishL2EndOfSequencingRequest(ctx context.Context, epoch uint64) error // TODO: add signer, sign request
	PublishPreconfirmationCommitment(ctx context.Context, sc SignedCommitment) error
	Close() error
}

type blockTopic struct {
	// blocks topic, main handle on block gossip
	topic *pubsub.Topic
	// block events handler, to be cancelled before closing the blocks topic.
	events *pubsub.TopicEventHandler
	// block subscriptions, to be cancelled before closing blocks topic.
	sub *pubsub.Subscription
}

func (bt *blockTopic) Close() error {
	bt.events.Cancel()
	bt.sub.Cancel()
	return bt.topic.Close()
}

type publisher struct {
	log log.Logger
	cfg *rollup.Config

	// p2pCancel cancels the downstream gossip event-handling functions, independent of the sources.
	// A closed gossip event source (event handler or subscription) does not stop any open event iteration,
	// thus we have to stop it ourselves this way.
	p2pCancel context.CancelFunc

	blocksV1 *blockTopic
	blocksV2 *blockTopic
	blocksV3 *blockTopic
	// CHANGE(taiko): add preconf blocks topic
	preconfBlocksV1                     *blockTopic
	preconfBlocksRequest                *blockTopic
	preconfBlocksResponse               *blockTopic
	preconfBlocksResponseV2             *blockTopic
	preconfBlocksEndOfSequencingRequest *blockTopic

	runCfg GossipRuntimeConfig
}

var _ GossipOut = (*publisher)(nil)

func combinePeers(allPeers ...[]peer.ID) []peer.ID {
	var seen = make(map[peer.ID]bool)
	var res []peer.ID
	for _, peers := range allPeers {
		for _, p := range peers {
			if _, ok := seen[p]; ok {
				continue
			}
			res = append(res, p)
			seen[p] = true
		}
	}
	return res
}

func (p *publisher) AllBlockTopicsPeers() []peer.ID {
	// CHANGE(taiko): combine preconf blocks topic peers.
	return combinePeers(p.BlocksTopicV1Peers(), p.BlocksTopicV2Peers(), p.BlocksTopicV3Peers(), p.PreconfBlocksTopicV1Peers())
}

func (p *publisher) BlocksTopicV1Peers() []peer.ID {
	return p.blocksV1.topic.ListPeers()
}

func (p *publisher) BlocksTopicV2Peers() []peer.ID {
	return p.blocksV2.topic.ListPeers()
}

func (p *publisher) BlocksTopicV3Peers() []peer.ID {
	return p.blocksV3.topic.ListPeers()
}

// CHANGE(taiko): get preconfBlocksV1 topic peers
func (p *publisher) PreconfBlocksTopicV1Peers() []peer.ID {
	return p.preconfBlocksV1.topic.ListPeers()
}

func (p *publisher) PublishL2Payload(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope, signer Signer) error {
	var payloadBuf bytes.Buffer
	if _, err := envelope.MarshalSSZ(&payloadBuf); err != nil {
		return fmt.Errorf("encode envelope (no sig): %w", err)
	}

	sigBytes, err := signer.Sign(
		ctx,
		SigningDomainBlocksV1,
		p.cfg.L2ChainID,
		payloadBuf.Bytes(),
	)
	if err != nil {
		return fmt.Errorf("sign execution payload: %w", err)
	}
	if len(sigBytes) != expectedSigLen {
		return fmt.Errorf("invalid signature length %d, want %d", len(sigBytes), expectedSigLen)
	}

	// CHANGE(taiko): now we have the envelope with the signature, encode it
	var fullBuf bytes.Buffer
	if _, err := envelope.MarshalSSZ(&fullBuf); err != nil {
		return fmt.Errorf("encode envelope (with sig): %w", err)
	}

	wireMsg := append(sigBytes[:], fullBuf.Bytes()...)

	out := snappy.Encode(nil, wireMsg)

	switch {
	case p.cfg.Taiko:
		return p.preconfBlocksV1.topic.Publish(ctx, out)
	case p.cfg.IsEcotone(uint64(envelope.ExecutionPayload.Timestamp)):
		return p.blocksV3.topic.Publish(ctx, out)
	case p.cfg.IsCanyon(uint64(envelope.ExecutionPayload.Timestamp)):
		return p.blocksV2.topic.Publish(ctx, out)
	default:
		return p.blocksV1.topic.Publish(ctx, out)
	}
}

// CHANGE(taiko): publish to preconfBlocksRequest topic
func (p *publisher) PublishL2Request(ctx context.Context, hash common.Hash) error {
	return p.preconfBlocksRequest.topic.Publish(ctx, hash.Bytes())
}

// CHANGE(taiko): publish to preconfBlocksEndOfSequencingRequest topic
func (p *publisher) PublishL2EndOfSequencingRequest(ctx context.Context, epoch uint64) error {
	// convert epoch to bytes
	epochBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(epochBytes, epoch)

	return p.preconfBlocksEndOfSequencingRequest.topic.Publish(ctx, epochBytes)
}

// CHANGE(taiko): publish to preconfBlocksResponse topic
func (p *publisher) PublishL2RequestResponse(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope, signer Signer) error {
	res := msgBufPool.Get().(*[]byte)
	buf := bytes.NewBuffer((*res)[:0])
	defer func() {
		*res = buf.Bytes()
		defer msgBufPool.Put(res)
	}()

	if _, err := envelope.MarshalSSZ(buf); err != nil {
		return fmt.Errorf("failed to encode execution payload envelope to publish: %w", err)
	}

	// CHANGE(taiko):
	// remove signing, Signer can be nil here now.
	// anyone can propagate blocks but the envelope.Signature will be read instead.
	data := buf.Bytes()

	// compress the full message (copies data into a new slice)
	out := snappy.Encode(nil, data)

	return p.preconfBlocksResponse.topic.Publish(ctx, out)
}

// CHANGE(taiko): publish SignedCommitment (preconfirmation) provided by sidecar using SSZ
func (p *publisher) PublishPreconfirmationCommitment(ctx context.Context, sc SignedCommitment) error {
	if len(sc.Signature) != expectedSigLen {
		return fmt.Errorf("invalid signature length %d, want %d", len(sc.Signature), expectedSigLen)
	}

	// Encode full SignedCommitment (commitment + signature)
	var fullBuf bytes.Buffer
	if _, err := sc.MarshalSSZ(&fullBuf); err != nil {
		return fmt.Errorf("encode signed preconf commitment: %w", err)
	}

	// Publish raw bytes (no snappy) on v2 response topic
	return p.preconfBlocksResponseV2.topic.Publish(ctx, fullBuf.Bytes())
}

func (p *publisher) Close() error {
	p.p2pCancel()
	e1 := p.blocksV1.Close()
	e2 := p.blocksV2.Close()
	return errors.Join(e1, e2)
}

func JoinGossip(self peer.ID, ps *pubsub.PubSub, log log.Logger, cfg *rollup.Config, runCfg GossipRuntimeConfig, gossipIn GossipIn) (GossipOut, error) {
	p2pCtx, p2pCancel := context.WithCancel(context.Background())

	v1Logger := log.New("topic", "blocksV1")
	blocksV1Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv1", v1Logger, BuildBlocksValidator(v1Logger, cfg, runCfg, eth.BlockV1)))
	blocksV1, err := newBlockTopic(p2pCtx, blocksTopicV1(cfg), ps, v1Logger, blocksV1Validator, gossipIn.OnUnsafeL2Payload)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v1 p2p: %w", err)
	}

	v2Logger := log.New("topic", "blocksV2")
	blocksV2Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv2", v2Logger, BuildBlocksValidator(v2Logger, cfg, runCfg, eth.BlockV2)))
	blocksV2, err := newBlockTopic(p2pCtx, blocksTopicV2(cfg), ps, v2Logger, blocksV2Validator, gossipIn.OnUnsafeL2Payload)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v2 p2p: %w", err)
	}

	v3Logger := log.New("topic", "blocksV3")
	blocksV3Validator := guardGossipValidator(log, logValidationResult(self, "validated blockv3", v3Logger, BuildBlocksValidator(v3Logger, cfg, runCfg, eth.BlockV3)))
	blocksV3, err := newBlockTopic(p2pCtx, blocksTopicV3(cfg), ps, v3Logger, blocksV3Validator, gossipIn.OnUnsafeL2Payload)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup blocks v3 p2p: %w", err)
	}

	// CHANGE(taiko): setup preconf blocks topic.
	preconfBlocksV1Logger := log.New("topic", "preconfBlocksV1")
	preconfBlocksV1Validator := guardGossipValidator(preconfBlocksV1Logger, logValidationResult(self, "validated preconfBlockv1", preconfBlocksV1Logger, BuildPreconfBlocksValidator(preconfBlocksV1Logger, cfg, runCfg, eth.BlockV1)))
	preconfBlocksV1, err := newBlockTopic(p2pCtx, preconfBlocksTopicV1(cfg), ps, preconfBlocksV1Logger, preconfBlocksV1Validator, gossipIn.OnUnsafeL2Payload)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup preconf blocks v1 p2p: %w", err)
	}

	// CHANGE(taiko): setup preconf blocks request topic (legacy hash requests)
	preconfBlocksRequestLogger := log.New("topic", "preconfBlockRequest")
	preconfBlocksRequestValidator := guardGossipValidator(preconfBlocksRequestLogger, logValidationResult(self, "validated preconfBlocksRequest", preconfBlocksRequestLogger, BuildPreconfBlocksRequestValidator(preconfBlocksRequestLogger, cfg, runCfg)))
	preconfBlocksRequest, err := newRequestTopic(p2pCtx, preconfBlocksRequestTopic(cfg), ps, preconfBlocksRequestLogger, gossipIn, preconfBlocksRequestValidator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup preconf blocks request p2p: %w", err)
	}

	// CHANGE(taiko): setup preconf blocks end of sequencing request topic
	preconfBlocksEndOfSequencingRequestLogger := log.New("topic", "preconfBlockEndOfSequencingRequest")
	preconfBlocksEndOfSequencingRequestValidator := guardGossipValidator(preconfBlocksEndOfSequencingRequestLogger, logValidationResult(self, "validated preconfBlocksEndOfSequencingRequest", preconfBlocksEndOfSequencingRequestLogger, BuildPreconfBlocksEndOfSequencingRequestValidator(preconfBlocksEndOfSequencingRequestLogger, cfg, runCfg)))
	preconfBlocksEndOfSequencingRequest, err := newEndOfSequencingRequestTopic(p2pCtx, preconfBlocksEndOfSequencingRequestTopic(cfg), ps, preconfBlocksEndOfSequencingRequestLogger, gossipIn, preconfBlocksEndOfSequencingRequestValidator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup preconf blocks end of sequencing request p2p: %w", err)
	}

	// CHANGE(taiko): setup preconf blocks response topic
	respLogger := log.New("topic", "preconfBlockResponse")
	respVal := guardGossipValidator(respLogger, logValidationResult(self, "validated preconfBlockResponse", respLogger, BuildPreconfBlocksResponseValidator(respLogger, cfg, runCfg, eth.BlockV1)))
	preconfBlocksResponse, err := newBlockTopic(p2pCtx, preconfBlocksResponseTopic(cfg), ps, respLogger, respVal, gossipIn.OnUnsafeL2Response)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup preconf blocks request p2p: %w", err)
	}

	// CHANGE(taiko): setup v2 preconfirmation topic (SignedCommitment)
	preconfsLogger := log.New("topic", "preconfirmations")
	preconfsValidator := guardGossipValidator(preconfsLogger, logValidationResult(self, "validated preconfirmation", preconfsLogger, BuildPreconfirmationValidator(preconfsLogger, cfg, runCfg)))
	preconfBlocksResponseV2, err := newPreconfirmationTopic(p2pCtx, preconfirmationsCommitmentTopic(cfg), ps, preconfsLogger, gossipIn, preconfsValidator)
	if err != nil {
		p2pCancel()
		return nil, fmt.Errorf("failed to setup preconf blocks response v2 (SignedCommitment) p2p: %w", err)
	}

	return &publisher{
		log:                                 log,
		cfg:                                 cfg,
		p2pCancel:                           p2pCancel,
		blocksV1:                            blocksV1,
		blocksV2:                            blocksV2,
		blocksV3:                            blocksV3,
		preconfBlocksV1:                     preconfBlocksV1,
		preconfBlocksRequest:                preconfBlocksRequest,
		preconfBlocksResponse:               preconfBlocksResponse,
		preconfBlocksResponseV2:             preconfBlocksResponseV2,
		preconfBlocksEndOfSequencingRequest: preconfBlocksEndOfSequencingRequest,
		runCfg:                              runCfg,
	}, nil
}

// CHANGE(taiko): create preconf blocks topic
func newRequestTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*blockTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	topic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	blocksTopicEvents, err := topic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create blocks gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, blocksTopicEvents)

	subscription, err := topic.Subscribe(pubsub.WithBufferSize(defaultBufferSize))
	if err != nil {
		err = errors.Join(err, topic.Close())
		return nil, fmt.Errorf("failed to subscribe to blocks gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, RequestsHandler(gossipIn.OnUnsafeL2Request))
	go subscriber(ctx, subscription)

	return &blockTopic{
		topic:  topic,
		events: blocksTopicEvents,
		sub:    subscription,
	}, nil
}

// CHANGE(taiko): create preconfirmation topic (SignedCommitment)
func newPreconfirmationTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*blockTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	topic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	topicEvents, err := topic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create preconfirmation gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, topicEvents)

	subscription, err := topic.Subscribe(pubsub.WithBufferSize(defaultBufferSize))
	if err != nil {
		err = errors.Join(err, topic.Close())
		return nil, fmt.Errorf("failed to subscribe to preconfirmation gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, PreconfirmationHandler(gossipIn.OnUnsafePreconfirmationCommitment))
	go subscriber(ctx, subscription)

	return &blockTopic{
		topic:  topic,
		events: topicEvents,
		sub:    subscription,
	}, nil
}

// CHANGE(taiko): create preconf blocks end of sequencing request topic
func newEndOfSequencingRequestTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, gossipIn GossipIn, validator pubsub.ValidatorEx) (*blockTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	topic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	blocksTopicEvents, err := topic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create blocks gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, blocksTopicEvents)

	subscription, err := topic.Subscribe(pubsub.WithBufferSize(defaultBufferSize))
	if err != nil {
		err = errors.Join(err, topic.Close())
		return nil, fmt.Errorf("failed to subscribe to blocks gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, EndOfSequencingRequestsHandler(gossipIn.OnUnsafeL2EndOfSequencingRequest))
	go subscriber(ctx, subscription)

	return &blockTopic{
		topic:  topic,
		events: blocksTopicEvents,
		sub:    subscription,
	}, nil
}

// CHANGE(taiko): create preconf blocks response topic
func newBlockTopic(ctx context.Context, topicId string, ps *pubsub.PubSub, log log.Logger, validator pubsub.ValidatorEx, handlerFunc func(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error) (*blockTopic, error) {
	err := ps.RegisterTopicValidator(topicId,
		validator,
		pubsub.WithValidatorTimeout(3*time.Second),
		pubsub.WithValidatorConcurrency(4))

	if err != nil {
		return nil, fmt.Errorf("failed to register gossip topic: %w", err)
	}

	blocksTopic, err := ps.Join(topicId)
	if err != nil {
		return nil, fmt.Errorf("failed to join gossip topic: %w", err)
	}

	blocksTopicEvents, err := blocksTopic.EventHandler()
	if err != nil {
		return nil, fmt.Errorf("failed to create blocks gossip topic handler: %w", err)
	}

	go LogTopicEvents(ctx, log, blocksTopicEvents)

	subscription, err := blocksTopic.Subscribe(pubsub.WithBufferSize(defaultBufferSize)) // CHANGE(taiko): change buffer size to defaultBufferSize
	if err != nil {
		err = errors.Join(err, blocksTopic.Close())
		return nil, fmt.Errorf("failed to subscribe to blocks gossip topic: %w", err)
	}

	subscriber := MakeSubscriber(log, BlocksHandler(handlerFunc))
	go subscriber(ctx, subscription)

	return &blockTopic{
		topic:  blocksTopic,
		events: blocksTopicEvents,
		sub:    subscription,
	}, nil
}

type TopicSubscriber func(ctx context.Context, sub *pubsub.Subscription)
type MessageHandler func(ctx context.Context, from peer.ID, msg any) error

func BlocksHandler(onBlock func(ctx context.Context, from peer.ID, msg *eth.ExecutionPayloadEnvelope) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		payload, ok := msg.(*eth.ExecutionPayloadEnvelope)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into execution payload, but got %T", msg)
		}
		return onBlock(ctx, from, payload)
	}
}

// CHANGE(taiko): create preconf blocks request handler
func RequestsHandler(onRequest func(ctx context.Context, from peer.ID, hash common.Hash) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		payload, ok := msg.(common.Hash)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into hash, but got %T", msg)
		}
		return onRequest(ctx, from, payload)
	}
}

// CHANGE(taiko): preconfirmation handler
func PreconfirmationHandler(on func(ctx context.Context, from peer.ID, msg *SignedCommitment) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		sc, ok := msg.(*SignedCommitment)
		if !ok {
			return fmt.Errorf("expected topic validator to parse SignedCommitment, but got %T", msg)
		}
		return on(ctx, from, sc)
	}
}

// CHANGE(taiko): create preconf blocks end of sequencing request handler
func EndOfSequencingRequestsHandler(onRequest func(ctx context.Context, from peer.ID, epoch uint64) error) MessageHandler {
	return func(ctx context.Context, from peer.ID, msg any) error {
		payload, ok := msg.(uint64)
		if !ok {
			return fmt.Errorf("expected topic validator to parse and validate data into uint64, but got %T", msg)
		}
		return onRequest(ctx, from, payload)
	}
}

func MakeSubscriber(log log.Logger, msgHandler MessageHandler) TopicSubscriber {
	return func(ctx context.Context, sub *pubsub.Subscription) {
		topicLog := log.New("topic", sub.Topic())
		for {
			msg, err := sub.Next(ctx)
			if err != nil { // ctx was closed, or subscription was closed
				topicLog.Debug("stopped subscriber")
				return
			}
			if msg.ValidatorData == nil {
				topicLog.Error("gossip message with no data", "from", msg.ReceivedFrom)
				continue
			}
			if err := msgHandler(ctx, msg.ReceivedFrom, msg.ValidatorData); err != nil {
				topicLog.Error("failed to process gossip message", "err", err)
			}
		}
	}
}

func LogTopicEvents(ctx context.Context, log log.Logger, evHandler *pubsub.TopicEventHandler) {
	for {
		ev, err := evHandler.NextPeerEvent(ctx)
		if err != nil {
			return // ctx closed
		}
		switch ev.Type {
		case pubsub.PeerJoin:
			log.Debug("peer joined topic", "peer", ev.Peer)
		case pubsub.PeerLeave:
			log.Debug("peer left topic", "peer", ev.Peer)
		default:
			log.Warn("unrecognized topic event", "ev", ev)
		}
	}
}

type gossipTracer struct {
	m GossipMetricer
}

func (g *gossipTracer) Trace(evt *pb.TraceEvent) {
	if g.m != nil {
		g.m.RecordGossipEvent(int32(*evt.Type))
	}
}
