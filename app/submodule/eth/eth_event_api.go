package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/venus/pkg/chain"
	"github.com/filecoin-project/venus/pkg/events"
	"github.com/filecoin-project/venus/pkg/events/filter"
	"github.com/filecoin-project/venus/pkg/statemanger"
	"github.com/filecoin-project/venus/venus-shared/api"
	v1 "github.com/filecoin-project/venus/venus-shared/api/chain/v1"
	"github.com/filecoin-project/venus/venus-shared/types"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	"github.com/zyedidia/generic/queue"
)

var (
	// wait for 3 epochs
	eventReadTimeout = 90 * time.Second
)

var _ v1.IETHEvent = (*ethEventAPI)(nil)

func newEthEventAPI(ctx context.Context, em *EthSubModule) (*ethEventAPI, error) {
	chainAPI := em.chainModule.API()
	cfg := em.cfg.FevmConfig
	ee := &ethEventAPI{
		em:                   em,
		ChainAPI:             chainAPI,
		MaxFilterHeightRange: abi.ChainEpoch(cfg.Event.MaxFilterHeightRange),
		SubscribtionCtx:      ctx,
		disable:              !cfg.EnableEthRPC || cfg.Event.DisableRealTimeFilterAPI,
	}

	if ee.disable {
		// all event functionality is disabled
		// the historic filter API relies on the real time one
		return ee, nil
	}

	ee.SubManager = &EthSubscriptionManager{
		ChainAPI:     chainAPI,
		stmgr:        ee.em.chainModule.Stmgr,
		messageStore: ee.em.chainModule.MessageStore,
	}
	ee.FilterStore = filter.NewMemFilterStore(cfg.Event.MaxFilters)

	// Enable indexing of actor events
	var eventIndex *filter.EventIndex
	if !cfg.Event.DisableHistoricFilterAPI {
		var dbPath string
		if len(cfg.Event.DatabasePath) == 0 {
			dbPath = filepath.Join(ee.em.sqlitePath, "events.db")
		} else {
			dbPath = cfg.Event.DatabasePath
		}

		var err error
		eventIndex, err = filter.NewEventIndex(ctx, dbPath, em.chainModule.ChainReader)
		if err != nil {
			return nil, err
		}
	}

	ee.EventFilterManager = &filter.EventFilterManager{
		MessageStore: ee.em.chainModule.MessageStore,
		ChainStore:   ee.em.chainModule.ChainReader,
		EventIndex:   eventIndex, // will be nil unless EnableHistoricFilterAPI is true
		AddressResolver: func(ctx context.Context, emitter abi.ActorID, ts *types.TipSet) (address.Address, bool) {
			// we only want to match using f4 addresses
			idAddr, err := address.NewIDAddress(uint64(emitter))
			if err != nil {
				return address.Undef, false
			}

			actor, err := em.chainModule.Stmgr.GetActorAt(ctx, idAddr, ts)
			if err != nil || actor.DelegatedAddress == nil {
				return idAddr, true
			}

			return *actor.DelegatedAddress, true
		},

		MaxFilterResults: cfg.Event.MaxFilterResults,
	}
	ee.TipSetFilterManager = &filter.TipSetFilterManager{
		MaxFilterResults: cfg.Event.MaxFilterResults,
	}
	ee.MemPoolFilterManager = &filter.MemPoolFilterManager{
		MaxFilterResults: cfg.Event.MaxFilterResults,
	}

	return ee, nil
}

type ethEventAPI struct {
	em                   *EthSubModule
	ChainAPI             v1.IChain
	EventFilterManager   *filter.EventFilterManager
	TipSetFilterManager  *filter.TipSetFilterManager
	MemPoolFilterManager *filter.MemPoolFilterManager
	FilterStore          filter.FilterStore
	SubManager           *EthSubscriptionManager
	MaxFilterHeightRange abi.ChainEpoch
	SubscribtionCtx      context.Context

	disable bool
}

func (e *ethEventAPI) Start(ctx context.Context) error {
	if e.disable {
		return nil
	}

	// Start garbage collection for filters
	go e.GC(ctx, time.Duration(e.em.cfg.FevmConfig.Event.FilterTTL))

	ev, err := events.NewEvents(ctx, e.ChainAPI)
	if err != nil {
		return err
	}
	// ignore returned tipsets
	_ = ev.Observe(e.EventFilterManager)
	_ = ev.Observe(e.TipSetFilterManager)

	ch, err := e.em.mpoolModule.MPool.Updates(ctx)
	if err != nil {
		return err
	}
	go e.MemPoolFilterManager.WaitForMpoolUpdates(ctx, ch)

	return nil
}

func (e *ethEventAPI) Close(ctx context.Context) error {
	if e.EventFilterManager != nil && e.EventFilterManager.EventIndex != nil {
		return e.EventFilterManager.EventIndex.Close()
	}

	return nil
}

// TODO: For now, we're fetching logs from the index for the entire block and then filtering them by the transaction hash
// This allows us to use the current schema of the event Index DB that has been optimised to use the "tipset_key_cid" index
// However, this can be replaced to filter logs in the event Index DB by the "msgCid" if we pass it down to the query generator
func (e *ethEventAPI) getEthLogsForBlockAndTransaction(ctx context.Context, blockHash *types.EthHash, txHash types.EthHash) ([]types.EthLog, error) {
	ces, err := e.ethGetEventsForFilter(ctx, &types.EthFilterSpec{BlockHash: blockHash})
	if err != nil {
		return nil, err
	}
	logs, err := ethFilterLogsFromEvents(ctx, ces, e.em.chainModule.MessageStore)
	if err != nil {
		return nil, err
	}
	var out []types.EthLog
	for _, log := range logs {
		if log.TransactionHash == txHash {
			out = append(out, log)
		}
	}
	return out, nil
}

func (e *ethEventAPI) EthGetLogs(ctx context.Context, filterSpec *types.EthFilterSpec) (*types.EthFilterResult, error) {
	ces, err := e.ethGetEventsForFilter(ctx, filterSpec)
	if err != nil {
		return nil, err
	}

	return ethFilterResultFromEvents(ctx, ces, e.em.chainModule.MessageStore)
}

func (e *ethEventAPI) ethGetEventsForFilter(ctx context.Context, filterSpec *types.EthFilterSpec) ([]*filter.CollectedEvent, error) {
	if e.EventFilterManager == nil {
		return nil, api.ErrNotSupported
	}

	if e.EventFilterManager.EventIndex == nil {
		return nil, fmt.Errorf("cannot use eth_get_logs if historical event index is disabled")
	}

	pf, err := e.parseEthFilterSpec(filterSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to parse eth filter spec: %w", err)
	}

	if pf.tipsetCid == cid.Undef {
		maxHeight := pf.maxHeight
		if maxHeight == -1 {
			// heaviest tipset doesn't have events because its messages haven't been executed yet
			maxHeight = e.em.chainModule.ChainReader.GetHead().Height() - 1
		}

		if maxHeight < 0 {
			return nil, fmt.Errorf("maxHeight requested is less than 0")
		}

		// we can't return events for the heaviest tipset as the transactions in that tipset will be executed
		// in the next non null tipset (because of Filecoin's "deferred execution" model)
		if maxHeight > e.em.chainModule.ChainReader.GetHead().Height()-1 {
			return nil, fmt.Errorf("maxHeight requested is greater than the heaviest tipset")
		}

		err := e.waitForHeightProcessed(ctx, maxHeight)
		if err != nil {
			return nil, err
		}
		// TODO: Ideally we should also check that events for the epoch at `pf.minheight` have been indexed
		// However, it is currently tricky to check/guarantee this for two reasons:
		// a) Event Index is not aware of null-blocks. This means that the Event Index wont be able to say whether the block at
		//    `pf.minheight` is a null block or whether it has no events
		// b) There can be holes in the index where events at certain epoch simply haven't been indexed because of edge cases around
		//    node restarts while indexing. This needs a long term "auto-repair"/"automated-backfilling" implementation in the index
		// So, for now, the best we can do is ensure that the event index has evenets for events at height >= `pf.maxHeight`
	} else {
		ts, err := e.em.chainModule.ChainReader.GetTipSetByCid(ctx, pf.tipsetCid)
		if err != nil {
			return nil, fmt.Errorf("failed to get tipset by cid: %w", err)
		}
		err = e.waitForHeightProcessed(ctx, ts.Height())
		if err != nil {
			return nil, err
		}

		b, err := e.EventFilterManager.EventIndex.IsTipsetProcessed(ctx, pf.tipsetCid.Bytes())
		if err != nil {
			return nil, fmt.Errorf("failed to check if tipset events have been indexed: %w", err)
		}
		if !b {
			return nil, fmt.Errorf("event index failed to index tipset %s", pf.tipsetCid.String())
		}
	}

	// Create a temporary filter
	f, err := e.EventFilterManager.Install(ctx, pf.minHeight, pf.maxHeight, pf.tipsetCid, pf.addresses, pf.keys, false)
	if err != nil {
		return nil, fmt.Errorf("failed to install event filter: %w", err)
	}
	ces := f.TakeCollectedEvents(ctx)

	_ = e.uninstallFilter(ctx, f)

	return ces, nil
}

// note that we can have null blocks at the given height and the event Index is not null block aware
// so, what we do here is wait till we see the event index contain a block at a height greater than the given height
func (e *ethEventAPI) waitForHeightProcessed(ctx context.Context, height abi.ChainEpoch) error {
	ei := e.EventFilterManager.EventIndex
	if height > e.em.chainModule.ChainReader.GetHead().Height() {
		return fmt.Errorf("height is in the future")
	}

	ctx, cancel := context.WithTimeout(ctx, eventReadTimeout)
	defer cancel()

	// if the height we're interested in has already been indexed -> there's nothing to do here
	if b, err := ei.IsHeightPast(ctx, uint64(height)); err != nil {
		return fmt.Errorf("failed to check if event index has events for given height: %w", err)
	} else if b {
		return nil
	}

	// subscribe for updates to the event index
	subCh, unSubscribeF := ei.SubscribeUpdates()
	defer unSubscribeF()

	// it could be that the event index was update while the subscription was being processed -> check if index has what we need now
	if b, err := ei.IsHeightPast(ctx, uint64(height)); err != nil {
		return fmt.Errorf("failed to check if event index has events for given height: %w", err)
	} else if b {
		return nil
	}

	for {
		select {
		case <-subCh:
			if b, err := ei.IsHeightPast(ctx, uint64(height)); err != nil {
				return fmt.Errorf("failed to check if event index has events for given height: %w", err)
			} else if b {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *ethEventAPI) EthGetFilterChanges(ctx context.Context, id types.EthFilterID) (*types.EthFilterResult, error) {
	if e.FilterStore == nil {
		return nil, api.ErrNotSupported
	}

	f, err := e.FilterStore.Get(ctx, types.FilterID(id))
	if err != nil {
		return nil, err
	}

	switch fc := f.(type) {
	case filterEventCollector:
		return ethFilterResultFromEvents(ctx, fc.TakeCollectedEvents(ctx), e.em.chainModule.MessageStore)
	case filterTipSetCollector:
		return ethFilterResultFromTipSets(fc.TakeCollectedTipSets(ctx))
	case filterMessageCollector:
		return ethFilterResultFromMessages(fc.TakeCollectedMessages(ctx))
	}

	return nil, fmt.Errorf("unknown filter type")
}

func (e *ethEventAPI) EthGetFilterLogs(ctx context.Context, id types.EthFilterID) (*types.EthFilterResult, error) {
	if e.FilterStore == nil {
		return nil, api.ErrNotSupported
	}

	f, err := e.FilterStore.Get(ctx, types.FilterID(id))
	if err != nil {
		return nil, err
	}

	switch fc := f.(type) {
	case filterEventCollector:
		return ethFilterResultFromEvents(ctx, fc.TakeCollectedEvents(ctx), e.em.chainModule.MessageStore)
	}

	return nil, fmt.Errorf("wrong filter type")
}

// parseBlockRange is similar to actor event's parseHeightRange but with slightly different semantics
//
// * "block" instead of "height"
// * strings that can have "latest" and "earliest" and nil
// * hex strings for actual heights
func parseBlockRange(heaviest abi.ChainEpoch, fromBlock, toBlock *string, maxRange abi.ChainEpoch) (minHeight abi.ChainEpoch, maxHeight abi.ChainEpoch, err error) {
	if fromBlock == nil || *fromBlock == "latest" || len(*fromBlock) == 0 {
		minHeight = heaviest
	} else if *fromBlock == "earliest" {
		minHeight = 0
	} else {
		if !strings.HasPrefix(*fromBlock, "0x") {
			return 0, 0, fmt.Errorf("FromBlock is not a hex")
		}
		epoch, err := types.EthUint64FromHex(*fromBlock)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid epoch")
		}
		minHeight = abi.ChainEpoch(epoch)
	}

	if toBlock == nil || *toBlock == "latest" || len(*toBlock) == 0 {
		// here latest means the latest at the time
		maxHeight = -1
	} else if *toBlock == "earliest" {
		maxHeight = 0
	} else {
		if !strings.HasPrefix(*toBlock, "0x") {
			return 0, 0, fmt.Errorf("ToBlock is not a hex")
		}
		epoch, err := types.EthUint64FromHex(*toBlock)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid epoch")
		}
		maxHeight = abi.ChainEpoch(epoch)
	}

	// Validate height ranges are within limits set by node operator
	if minHeight == -1 && maxHeight > 0 {
		// Here the client is looking for events between the head and some future height
		if maxHeight-heaviest > maxRange {
			return 0, 0, fmt.Errorf("invalid epoch range: to block is too far in the future (maximum: %d)", maxRange)
		}
	} else if minHeight >= 0 && maxHeight == -1 {
		// Here the client is looking for events between some time in the past and the current head
		if heaviest-minHeight > maxRange {
			return 0, 0, fmt.Errorf("invalid epoch range: from block is too far in the past (maximum: %d)", maxRange)
		}
	} else if minHeight >= 0 && maxHeight >= 0 {
		if minHeight > maxHeight {
			return 0, 0, fmt.Errorf("invalid epoch range: to block (%d) must be after from block (%d)", minHeight, maxHeight)
		} else if maxHeight-minHeight > maxRange {
			return 0, 0, fmt.Errorf("invalid epoch range: range between to and from blocks is too large (maximum: %d)", maxRange)
		}
	}
	return minHeight, maxHeight, nil
}

func (e *ethEventAPI) installEthFilterSpec(ctx context.Context, filterSpec *types.EthFilterSpec) (filter.EventFilter, error) {
	var (
		minHeight abi.ChainEpoch
		maxHeight abi.ChainEpoch
		tipsetCid cid.Cid
		addresses []address.Address
		keys      = map[string][][]byte{}
	)

	if filterSpec.BlockHash != nil {
		if filterSpec.FromBlock != nil || filterSpec.ToBlock != nil {
			return nil, fmt.Errorf("must not specify block hash and from/to block")
		}

		tipsetCid = filterSpec.BlockHash.ToCid()
	} else {
		head, err := e.ChainAPI.ChainHead(ctx)
		if err != nil {
			return nil, err
		}
		minHeight, maxHeight, err = parseBlockRange(head.Height(), filterSpec.FromBlock, filterSpec.ToBlock, e.MaxFilterHeightRange)
		if err != nil {
			return nil, err
		}
	}

	// Convert all addresses to filecoin f4 addresses
	for _, ea := range filterSpec.Address {
		a, err := ea.ToFilecoinAddress()
		if err != nil {
			return nil, fmt.Errorf("invalid address %x", ea)
		}
		addresses = append(addresses, a)
	}

	keys, err := parseEthTopics(filterSpec.Topics)
	if err != nil {
		return nil, err
	}

	return e.EventFilterManager.Install(ctx, minHeight, maxHeight, tipsetCid, addresses, keysToKeysWithCodec(keys), true)
}

func keysToKeysWithCodec(keys map[string][][]byte) map[string][]types.ActorEventBlock {
	keysWithCodec := make(map[string][]types.ActorEventBlock)
	for k, v := range keys {
		for _, vv := range v {
			keysWithCodec[k] = append(keysWithCodec[k], types.ActorEventBlock{
				Codec: uint64(multicodec.Raw), // FEVM smart contract events are always encoded with the `raw` Codec.
				Value: vv,
			})
		}
	}
	return keysWithCodec
}

func (e *ethEventAPI) EthNewFilter(ctx context.Context, filterSpec *types.EthFilterSpec) (types.EthFilterID, error) {
	if e.FilterStore == nil || e.EventFilterManager == nil {
		return types.EthFilterID{}, api.ErrNotSupported
	}

	f, err := e.installEthFilterSpec(ctx, filterSpec)
	if err != nil {
		return types.EthFilterID{}, err
	}

	if err := e.FilterStore.Add(ctx, f); err != nil {
		// Could not record in store, attempt to delete filter to clean up
		err2 := e.EventFilterManager.Remove(ctx, f.ID())
		if err2 != nil {
			return types.EthFilterID{}, fmt.Errorf("encountered error %v while removing new filter due to %v", err2, err)
		}

		return types.EthFilterID{}, err
	}

	return types.EthFilterID(f.ID()), nil
}

func (e *ethEventAPI) EthNewBlockFilter(ctx context.Context) (types.EthFilterID, error) {
	if e.FilterStore == nil || e.TipSetFilterManager == nil {
		return types.EthFilterID{}, api.ErrNotSupported
	}

	f, err := e.TipSetFilterManager.Install(ctx)
	if err != nil {
		return types.EthFilterID{}, err
	}

	if err := e.FilterStore.Add(ctx, f); err != nil {
		// Could not record in store, attempt to delete filter to clean up
		err2 := e.TipSetFilterManager.Remove(ctx, f.ID())
		if err2 != nil {
			return types.EthFilterID{}, fmt.Errorf("encountered error %v while removing new filter due to %v", err2, err)
		}

		return types.EthFilterID{}, err
	}

	return types.EthFilterID(f.ID()), nil
}

func (e *ethEventAPI) EthNewPendingTransactionFilter(ctx context.Context) (types.EthFilterID, error) {
	if e.FilterStore == nil || e.MemPoolFilterManager == nil {
		return types.EthFilterID{}, api.ErrNotSupported
	}

	f, err := e.MemPoolFilterManager.Install(ctx)
	if err != nil {
		return types.EthFilterID{}, err
	}

	if err := e.FilterStore.Add(ctx, f); err != nil {
		// Could not record in store, attempt to delete filter to clean up
		err2 := e.MemPoolFilterManager.Remove(ctx, f.ID())
		if err2 != nil {
			return types.EthFilterID{}, fmt.Errorf("encountered error %v while removing new filter due to %v", err2, err)
		}

		return types.EthFilterID{}, err
	}

	return types.EthFilterID(f.ID()), nil
}

func (e *ethEventAPI) EthUninstallFilter(ctx context.Context, id types.EthFilterID) (bool, error) {
	if e.FilterStore == nil {
		return false, api.ErrNotSupported
	}

	f, err := e.FilterStore.Get(ctx, types.FilterID(id))
	if err != nil {
		if errors.Is(err, filter.ErrFilterNotFound) {
			return false, nil
		}
		return false, err
	}

	if err := e.uninstallFilter(ctx, f); err != nil {
		return false, err
	}

	return true, nil
}

func (e *ethEventAPI) uninstallFilter(ctx context.Context, f filter.Filter) error {
	switch f.(type) {
	case filter.EventFilter:
		err := e.EventFilterManager.Remove(ctx, f.ID())
		if err != nil && !errors.Is(err, filter.ErrFilterNotFound) {
			return err
		}
	case *filter.TipSetFilter:
		err := e.TipSetFilterManager.Remove(ctx, f.ID())
		if err != nil && !errors.Is(err, filter.ErrFilterNotFound) {
			return err
		}
	case *filter.MemPoolFilter:
		err := e.MemPoolFilterManager.Remove(ctx, f.ID())
		if err != nil && !errors.Is(err, filter.ErrFilterNotFound) {
			return err
		}
	default:
		return fmt.Errorf("unknown filter type")
	}

	return e.FilterStore.Remove(ctx, f.ID())
}

const (
	EthSubscribeEventTypeHeads               = "newHeads"
	EthSubscribeEventTypeLogs                = "logs"
	EthSubscribeEventTypePendingTransactions = "newPendingTransactions"
)

func (e *ethEventAPI) EthSubscribe(ctx context.Context, p jsonrpc.RawParams) (types.EthSubscriptionID, error) {
	params, err := jsonrpc.DecodeParams[types.EthSubscribeParams](p)
	if err != nil {
		return types.EthSubscriptionID{}, fmt.Errorf("decoding params: %w", err)
	}
	if e.SubManager == nil {
		return types.EthSubscriptionID{}, api.ErrNotSupported
	}
	// Note that go-jsonrpc will set the method field of the response to "xrpc.ch.val" but the ethereum api expects the name of the
	// method to be "eth_subscription". This probably doesn't matter in practice.

	ethCb, ok := jsonrpc.ExtractReverseClient[v1.EthSubscriberMethods](ctx)
	if !ok {
		return types.EthSubscriptionID{}, fmt.Errorf("connection doesn't support callbacks")
	}

	sub, err := e.SubManager.StartSubscription(e.SubscribtionCtx, ethCb.EthSubscription, e.uninstallFilter)
	if err != nil {
		return types.EthSubscriptionID{}, err
	}

	switch params.EventType {
	case EthSubscribeEventTypeHeads:
		f, err := e.TipSetFilterManager.Install(ctx)
		if err != nil {
			// clean up any previous filters added and stop the sub
			_, _ = e.EthUnsubscribe(ctx, sub.id)
			return types.EthSubscriptionID{}, err
		}
		sub.addFilter(ctx, f)

	case EthSubscribeEventTypeLogs:
		keys := map[string][][]byte{}
		if params.Params != nil {
			var err error
			keys, err = parseEthTopics(params.Params.Topics)
			if err != nil {
				// clean up any previous filters added and stop the sub
				_, _ = e.EthUnsubscribe(ctx, sub.id)
				return types.EthSubscriptionID{}, err
			}
		}

		var addresses []address.Address
		if params.Params != nil {
			for _, ea := range params.Params.Address {
				a, err := ea.ToFilecoinAddress()
				if err != nil {
					return types.EthSubscriptionID{}, fmt.Errorf("invalid address %x", ea)
				}
				addresses = append(addresses, a)
			}
		}

		f, err := e.EventFilterManager.Install(ctx, -1, -1, cid.Undef, addresses, keysToKeysWithCodec(keys), true)
		if err != nil {
			// clean up any previous filters added and stop the sub
			_, _ = e.EthUnsubscribe(ctx, sub.id)
			return types.EthSubscriptionID{}, err
		}
		sub.addFilter(ctx, f)

	case EthSubscribeEventTypePendingTransactions:
		f, err := e.MemPoolFilterManager.Install(ctx)
		if err != nil {
			// clean up any previous filters added and stop the sub
			_, _ = e.EthUnsubscribe(ctx, sub.id)
			return types.EthSubscriptionID{}, err
		}

		sub.addFilter(ctx, f)
	default:
		return types.EthSubscriptionID{}, fmt.Errorf("unsupported event type: %s", params.EventType)
	}

	return sub.id, nil
}

func (e *ethEventAPI) EthUnsubscribe(ctx context.Context, id types.EthSubscriptionID) (bool, error) {
	if e.SubManager == nil {
		return false, api.ErrNotSupported
	}

	err := e.SubManager.StopSubscription(ctx, id)
	if err != nil {
		return false, nil
	}

	return true, nil
}

// GC runs a garbage collection loop, deleting filters that have not been used within the ttl window
func (e *ethEventAPI) GC(ctx context.Context, ttl time.Duration) {
	if e.FilterStore == nil {
		return
	}

	tt := time.NewTicker(time.Minute * 30)
	defer tt.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tt.C:
			fs := e.FilterStore.NotTakenSince(time.Now().Add(-ttl))
			for _, f := range fs {
				if err := e.uninstallFilter(ctx, f); err != nil {
					log.Warnf("Failed to remove actor event filter during garbage collection: %v", err)
				}
			}
		}
	}
}

type parsedFilter struct {
	minHeight abi.ChainEpoch
	maxHeight abi.ChainEpoch
	tipsetCid cid.Cid
	addresses []address.Address
	keys      map[string][]types.ActorEventBlock
}

func (e *ethEventAPI) parseEthFilterSpec(filterSpec *types.EthFilterSpec) (*parsedFilter, error) {
	var (
		minHeight abi.ChainEpoch
		maxHeight abi.ChainEpoch
		tipsetCid cid.Cid
		addresses []address.Address
		keys      = map[string][][]byte{}
	)

	if filterSpec.BlockHash != nil {
		if filterSpec.FromBlock != nil || filterSpec.ToBlock != nil {
			return nil, fmt.Errorf("must not specify block hash and from/to block")
		}

		tipsetCid = filterSpec.BlockHash.ToCid()
	} else {
		var err error
		head := e.em.chainModule.ChainReader.GetHead()
		minHeight, maxHeight, err = parseBlockRange(head.Height(), filterSpec.FromBlock, filterSpec.ToBlock, e.MaxFilterHeightRange)
		if err != nil {
			return nil, err
		}
	}

	// Convert all addresses to filecoin f4 addresses
	for _, ea := range filterSpec.Address {
		a, err := ea.ToFilecoinAddress()
		if err != nil {
			return nil, fmt.Errorf("invalid address %x", ea)
		}
		addresses = append(addresses, a)
	}

	keys, err := parseEthTopics(filterSpec.Topics)
	if err != nil {
		return nil, err
	}

	return &parsedFilter{
		minHeight: minHeight,
		maxHeight: maxHeight,
		tipsetCid: tipsetCid,
		addresses: addresses,
		keys:      keysToKeysWithCodec(keys),
	}, nil
}

type filterEventCollector interface {
	TakeCollectedEvents(context.Context) []*filter.CollectedEvent
}

type filterMessageCollector interface {
	TakeCollectedMessages(context.Context) []*types.SignedMessage
}

type filterTipSetCollector interface {
	TakeCollectedTipSets(context.Context) []types.TipSetKey
}

func ethLogFromEvent(entries []types.EventEntry) (data []byte, topics []types.EthHash, ok bool) {
	var (
		topicsFound      [4]bool
		topicsFoundCount int
		dataFound        bool
	)
	topics = make([]types.EthHash, 0, 4)
	for _, entry := range entries {
		// Drop events with non-raw topics. Built-in actors emit CBOR, and anything else would be
		// invalid anyway.
		if entry.Codec != cid.Raw {
			return nil, nil, false
		}
		// Check if the key is t1..t4
		if len(entry.Key) == 2 && "t1" <= entry.Key && entry.Key <= "t4" {
			// '1' - '1' == 0, etc.
			idx := int(entry.Key[1] - '1')

			// Drop events with mis-sized topics.
			if len(entry.Value) != 32 {
				log.Warnw("got an EVM event topic with an invalid size", "key", entry.Key, "size", len(entry.Value))
				return nil, nil, false
			}

			// Drop events with duplicate topics.
			if topicsFound[idx] {
				log.Warnw("got a duplicate EVM event topic", "key", entry.Key)
				return nil, nil, false
			}
			topicsFound[idx] = true
			topicsFoundCount++

			// Extend the topics array
			for len(topics) <= idx {
				topics = append(topics, types.EthHash{})
			}

			copy(topics[idx][:], entry.Value)
		} else if entry.Key == "d" {
			// Drop events with duplicate data fields.
			if dataFound {
				log.Warnw("got duplicate EVM event data")
				return nil, nil, false
			}

			dataFound = true
			data = entry.Value
		} else {
			// Skip entries we don't understand (makes it easier to extend things).
			// But we warn for now because we don't expect them.
			log.Warnw("unexpected event entry", "key", entry.Key)
		}

	}

	// Drop events with skipped topics.
	if len(topics) != topicsFoundCount {
		log.Warnw("EVM event topic length mismatch", "expected", len(topics), "actual", topicsFoundCount)
		return nil, nil, false
	}
	return data, topics, true
}

// func ethFilterResultFromEvents(evs []*filter.CollectedEvent, ms *chain.MessageStore) (*types.EthFilterResult, error) {
func ethFilterLogsFromEvents(_ context.Context, evs []*filter.CollectedEvent, ms *chain.MessageStore) ([]types.EthLog, error) {
	var logs []types.EthLog
	for _, ev := range evs {
		log := types.EthLog{
			Removed:          ev.Reverted,
			LogIndex:         types.EthUint64(ev.EventIdx),
			TransactionIndex: types.EthUint64(ev.MsgIdx),
			BlockNumber:      types.EthUint64(ev.Height),
		}
		var (
			err error
			ok  bool
		)

		log.Data, log.Topics, ok = ethLogFromEvent(ev.Entries)
		if !ok {
			continue
		}

		log.Address, err = types.EthAddressFromFilecoinAddress(ev.EmitterAddr)
		if err != nil {
			return nil, err
		}

		log.TransactionHash, err = ethTxHashFromMessageCid(context.TODO(), ev.MsgCid, ms)
		if err != nil {
			return nil, err
		}
		if log.TransactionHash == types.EmptyEthHash {
			// We've garbage collected the message, ignore the events and continue.
			continue
		}

		c, err := ev.TipSetKey.Cid()
		if err != nil {
			return nil, err
		}
		log.BlockHash, err = types.EthHashFromCid(c)
		if err != nil {
			return nil, err
		}

		logs = append(logs, log)
	}

	return logs, nil
}

func ethFilterResultFromEvents(ctx context.Context, evs []*filter.CollectedEvent, ms *chain.MessageStore) (*types.EthFilterResult, error) {
	logs, err := ethFilterLogsFromEvents(ctx, evs, ms)
	if err != nil {
		return nil, err
	}

	res := &types.EthFilterResult{}
	for _, log := range logs {
		res.Results = append(res.Results, log)
	}

	return res, nil
}

func ethFilterResultFromTipSets(tsks []types.TipSetKey) (*types.EthFilterResult, error) {
	res := &types.EthFilterResult{}

	for _, tsk := range tsks {
		c, err := tsk.Cid()
		if err != nil {
			return nil, err
		}
		hash, err := types.EthHashFromCid(c)
		if err != nil {
			return nil, err
		}

		res.Results = append(res.Results, hash)
	}

	return res, nil
}

func ethFilterResultFromMessages(cs []*types.SignedMessage) (*types.EthFilterResult, error) {
	res := &types.EthFilterResult{}

	for _, c := range cs {
		hash, err := ethTxHashFromSignedMessage(c)
		if err != nil {
			return nil, err
		}

		res.Results = append(res.Results, hash)
	}

	return res, nil
}

type EthSubscriptionManager struct { // nolint
	ChainAPI     v1.IChain
	messageStore *chain.MessageStore
	stmgr        *statemanger.Stmgr
	mu           sync.Mutex
	subs         map[types.EthSubscriptionID]*ethSubscription
}

func (e *EthSubscriptionManager) StartSubscription(ctx context.Context, out ethSubscriptionCallback, dropFilter func(context.Context, filter.Filter) error) (*ethSubscription, error) { // nolint
	rawid, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("new uuid: %w", err)
	}
	id := types.EthSubscriptionID{}
	copy(id[:], rawid[:]) // uuid is 16 bytes

	ctx, quit := context.WithCancel(ctx)

	sub := &ethSubscription{
		chainAPI:        e.ChainAPI,
		stmgr:           e.stmgr,
		messageStore:    e.messageStore,
		uninstallFilter: dropFilter,
		id:              id,
		in:              make(chan interface{}, 200),
		out:             out,
		quit:            quit,

		toSend:   queue.New[[]byte](),
		sendCond: make(chan struct{}, 1),
	}

	e.mu.Lock()
	if e.subs == nil {
		e.subs = make(map[types.EthSubscriptionID]*ethSubscription)
	}
	e.subs[sub.id] = sub
	e.mu.Unlock()

	go sub.start(ctx)
	go sub.startOut(ctx)

	return sub, nil
}

func (e *EthSubscriptionManager) StopSubscription(ctx context.Context, id types.EthSubscriptionID) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	sub, ok := e.subs[id]
	if !ok {
		return fmt.Errorf("subscription not found")
	}
	sub.stop()
	delete(e.subs, id)

	return nil
}

type ethSubscriptionCallback func(context.Context, jsonrpc.RawParams) error

const maxSendQueue = 20000

type ethSubscription struct {
	chainAPI        v1.IChain
	stmgr           *statemanger.Stmgr
	messageStore    *chain.MessageStore
	uninstallFilter func(context.Context, filter.Filter) error
	id              types.EthSubscriptionID
	in              chan interface{}
	out             ethSubscriptionCallback

	mu      sync.Mutex
	filters []filter.Filter
	quit    func()

	sendLk       sync.Mutex
	sendQueueLen int
	toSend       *queue.Queue[[]byte]
	sendCond     chan struct{}

	lastSentTipset *types.TipSetKey
}

func (e *ethSubscription) addFilter(_ context.Context, f filter.Filter) {
	e.mu.Lock()
	defer e.mu.Unlock()

	f.SetSubChannel(e.in)
	e.filters = append(e.filters, f)
}

// startOut processes the final subscription queue. It's here in case the subscriber
// is slow, and we need to buffer the messages.
func (e *ethSubscription) startOut(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.sendCond:
			e.sendLk.Lock()

			for !e.toSend.Empty() {
				front := e.toSend.Dequeue()
				e.sendQueueLen--

				e.sendLk.Unlock()

				if err := e.out(ctx, front); err != nil {
					log.Warnw("error sending subscription response, killing subscription", "sub", e.id, "error", err)
					e.stop()
					return
				}

				e.sendLk.Lock()
			}

			e.sendLk.Unlock()
		}
	}
}

func (e *ethSubscription) send(_ context.Context, v interface{}) {
	resp := types.EthSubscriptionResponse{
		SubscriptionID: e.id,
		Result:         v,
	}

	outParam, err := json.Marshal(resp)
	if err != nil {
		log.Warnw("marshaling subscription response", "sub", e.id, "error", err)
		return
	}

	e.sendLk.Lock()
	defer e.sendLk.Unlock()

	e.toSend.Enqueue(outParam)

	e.sendQueueLen++
	if e.sendQueueLen > maxSendQueue {
		log.Warnw("subscription send queue full, killing subscription", "sub", e.id)
		e.stop()
		return
	}

	select {
	case e.sendCond <- struct{}{}:
	default: // already signalled, and we're holding the lock so we know that the event will be processed
	}
}

func (e *ethSubscription) start(ctx context.Context) {
	if ctx.Err() == nil {
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-e.in:
				switch vt := v.(type) {
				case *filter.CollectedEvent:
					evs, err := ethFilterResultFromEvents(ctx, []*filter.CollectedEvent{vt}, e.messageStore)
					if err != nil {
						continue
					}

					for _, r := range evs.Results {
						e.send(ctx, r)
					}
				case *types.TipSet:
					// Skip processing for tipset at epoch 0 as it has no parent
					if vt.Height() == 0 {
						continue
					}
					// Check if the parent has already been processed
					parentTipSetKey := vt.Parents()
					if e.lastSentTipset != nil && (*e.lastSentTipset) == parentTipSetKey {
						continue
					}
					parentTipSet, loadErr := e.chainAPI.ChainGetTipSet(ctx, parentTipSetKey)
					if loadErr != nil {
						log.Warnw("failed to load parent tipset", "tipset", parentTipSetKey, "error", loadErr)
						continue
					}
					ethBlock, ethBlockErr := newEthBlockFromFilecoinTipSet(ctx, parentTipSet, true, e.messageStore, e.stmgr)
					if ethBlockErr != nil {
						continue
					}

					e.send(ctx, ethBlock)
					e.lastSentTipset = &parentTipSetKey
				case *types.SignedMessage: // mpool txid
					evs, err := ethFilterResultFromMessages([]*types.SignedMessage{vt})
					if err != nil {
						continue
					}

					for _, r := range evs.Results {
						e.send(ctx, r)
					}
				default:
					log.Warnf("unexpected subscription value type: %T", vt)
				}
			}
		}
	}
}

func (e *ethSubscription) stop() {
	e.mu.Lock()
	if e.quit == nil {
		e.mu.Unlock()
		return
	}

	if e.quit != nil {
		e.quit()
		e.quit = nil
		e.mu.Unlock()

		for _, f := range e.filters {
			// note: the context in actually unused in uninstallFilter
			if err := e.uninstallFilter(context.TODO(), f); err != nil {
				// this will leave the filter a zombie, collecting events up to the maximum allowed
				log.Warnf("failed to remove filter when unsubscribing: %v", err)
			}
		}
	}
}
