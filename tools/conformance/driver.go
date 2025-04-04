package conformance

import (
	"context"
	gobig "math/big"
	"os"

	"github.com/filecoin-project/venus/pkg/consensus"
	"github.com/filecoin-project/venus/pkg/consensus/chainselector"
	"github.com/filecoin-project/venus/pkg/fvm"
	"github.com/filecoin-project/venus/pkg/state"
	"github.com/filecoin-project/venus/pkg/util/ffiwrapper/impl"
	"github.com/filecoin-project/venus/pkg/vm/gas"
	"github.com/filecoin-project/venus/pkg/vm/vmcontext"
	blockstoreutil "github.com/filecoin-project/venus/venus-shared/blockstore"
	cbor "github.com/ipfs/go-ipld-cbor"

	"github.com/filecoin-project/venus/app/node"
	"github.com/filecoin-project/venus/fixtures/networks"
	"github.com/filecoin-project/venus/pkg/chain"
	"github.com/filecoin-project/venus/pkg/consensusfault"
	_ "github.com/filecoin-project/venus/pkg/crypto/bls"       // enable bls signatures
	_ "github.com/filecoin-project/venus/pkg/crypto/delegated" // enable delegated signatures
	_ "github.com/filecoin-project/venus/pkg/crypto/secp"      // enable secp signatures
	"github.com/filecoin-project/venus/pkg/fork"
	"github.com/filecoin-project/venus/pkg/state/tree"
	"github.com/filecoin-project/venus/pkg/vm"
	"github.com/filecoin-project/venus/pkg/vm/register"
	"github.com/filecoin-project/venus/pkg/vmsupport"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/network"
	rtt "github.com/filecoin-project/go-state-types/rt"
	"github.com/filecoin-project/test-vectors/schema"
	"github.com/filecoin-project/venus/tools/conformance/chaos"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin"
	"github.com/filecoin-project/venus/venus-shared/types"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
)

var (
	// DefaultCirculatingSupply is the fallback circulating supply returned by
	// the driver's CircSupplyCalculator function, used if the vector specifies
	// no circulating supply.
	DefaultCirculatingSupply = types.TotalFilecoinInt

	// DefaultBaseFee to use in the VM, if one is not supplied in the vector.
	DefaultBaseFee = abi.NewTokenAmount(100)
)

type Driver struct {
	ctx      context.Context
	selector schema.Selector
	vmFlush  bool
}

type DriverOpts struct {
	// DisableVMFlush, when true, avoids calling LegacyVM.Flush(), forces a blockstore
	// recursive copy, from the temporary buffer blockstore, to the real
	// system's blockstore. Disabling LegacyVM flushing is useful when extracting test
	// vectors and trimming state, as we don't want to force an accidental
	// deep copy of the state tree.
	//
	// Disabling LegacyVM flushing almost always should go hand-in-hand with
	// LOTUS_DISABLE_VM_BUF=iknowitsabadidea. That way, state tree writes are
	// immediately committed to the blockstore.
	DisableVMFlush bool
}

func NewDriver(ctx context.Context, selector schema.Selector, opts DriverOpts) *Driver {
	return &Driver{ctx: ctx, selector: selector, vmFlush: !opts.DisableVMFlush}
}

type ExecuteTipsetResult struct {
	ReceiptsRoot  cid.Cid
	PostStateRoot cid.Cid

	// AppliedMessages stores the messages that were applied, in the order they
	// were applied. It includes implicit messages (cron, rewards).
	AppliedMessages []*types.Message
	// AppliedResults stores the results of AppliedMessages, in the same order.
	AppliedResults []*vm.Ret
}

// ExecuteTipset executes the supplied tipset on top of the state represented
// by the preroot CID.
//
// parentEpoch is the last epoch in which an actual tipset was processed. This
// is used by Lotus for null block counting and cron firing.
//
// This method returns the receipts root, the poststate root, and the LegacyVM
// message results. The latter _include_ implicit messages, such as cron ticks
// and reward withdrawal per miner.
func (d *Driver) ExecuteTipset(bs blockstoreutil.Blockstore, chainDs ds.Batching, preroot cid.Cid, parentEpoch abi.ChainEpoch, tipset *schema.Tipset, execEpoch abi.ChainEpoch) (*ExecuteTipsetResult, error) {
	ipldStore := cbor.NewCborStore(bs)
	mainNetParams := networks.Mainnet()
	node.SetNetParams(&mainNetParams.Network)
	// chainstore
	chainStore := chain.NewStore(chainDs, bs, cid.Undef, chainselector.Weight) // load genesis from car

	// chain fork
	chainFork, err := fork.NewChainFork(context.TODO(), chainStore, ipldStore, bs, &mainNetParams.Network, chainDs)
	faultChecker := consensusfault.NewFaultChecker(chainStore, chainFork)
	syscalls := vmsupport.NewSyscalls(faultChecker, impl.ProofVerifier)
	if err != nil {
		return nil, err
	}

	var (
		ctx      = context.Background()
		vmOption = vm.VmOption{
			CircSupplyCalculator: func(context.Context, abi.ChainEpoch, tree.Tree) (abi.TokenAmount, error) {
				return big.Zero(), nil
			},
			LookbackStateGetter: vmcontext.LookbackStateGetterForTipset(ctx, chainStore, chainFork, nil),
			NetworkVersion:      chainFork.GetNetworkVersion(ctx, execEpoch),
			Rnd:                 NewFixedRand(),
			BaseFee:             big.NewFromGo(&tipset.BaseFee),
			Fork:                chainFork,
			Epoch:               execEpoch,
			GasPriceSchedule:    gas.NewPricesSchedule(mainNetParams.Network.ForkUpgradeParam),
			PRoot:               preroot,
			Bsstore:             bs,
			SysCallsImpl:        syscalls,
			TipSetGetter:        vmcontext.TipSetGetterForTipset(chainStore.GetTipSetByHeight, nil),
			Tracing:             true,
			ActorDebugging:      mainNetParams.Network.ActorDebugging,
		}
	)

	blocks := make([]types.BlockMessagesInfo, 0, len(tipset.Blocks))
	for _, b := range tipset.Blocks {
		sb := types.BlockMessagesInfo{
			Block: &types.BlockHeader{
				Miner: b.MinerAddr,
				ElectionProof: &types.ElectionProof{
					WinCount: b.WinCount,
				},
			},
		}
		for _, m := range b.Messages {
			msg, err := types.DecodeMessage(m)
			if err != nil {
				return nil, err
			}
			switch msg.From.Protocol() {
			case address.SECP256K1:
				sb.SecpkMessages = append(sb.SecpkMessages, &types.SignedMessage{
					Message: *msg,
					Signature: crypto.Signature{
						Type: crypto.SigTypeSecp256k1,
						Data: make([]byte, 65),
					},
				})
			case address.BLS:
				sb.BlsMessages = append(sb.BlsMessages, msg)
			default:
				// sneak in messages originating from other addresses as both kinds.
				// these should fail, as they are actually invalid senders.
				sb.SecpkMessages = append(sb.SecpkMessages, msg) // todo  use interface for message
				sb.BlsMessages = append(sb.BlsMessages, msg)
			}
		}
		blocks = append(blocks, sb)
	}

	var (
		messages []*types.Message
		results  []*vm.Ret
	)

	circulatingSupplyCalculator := chain.NewCirculatingSupplyCalculator(bs, preroot, &mainNetParams.Network, chainFork.GetNetworkVersion)
	processor := consensus.NewDefaultProcessor(syscalls, circulatingSupplyCalculator, chainStore, &mainNetParams.Network)

	postcid, receipt, err := processor.ApplyBlocks(ctx, blocks, nil, preroot, parentEpoch, execEpoch, vmOption, func(_ cid.Cid, msg *types.Message, ret *vm.Ret) error {
		messages = append(messages, msg)
		results = append(results, ret)
		return nil
	})
	if err != nil {
		return nil, err
	}
	receiptsroot, err := chain.GetReceiptRoot(receipt)
	if err != nil {
		return nil, err
	}

	ret := &ExecuteTipsetResult{
		ReceiptsRoot:    receiptsroot,
		PostStateRoot:   postcid,
		AppliedMessages: messages,
		AppliedResults:  results,
	}
	return ret, nil
}

type ExecuteMessageParams struct {
	Preroot        cid.Cid
	Epoch          abi.ChainEpoch
	Timestamp      uint64
	Message        *types.Message
	CircSupply     abi.TokenAmount
	BaseFee        abi.TokenAmount
	NetworkVersion network.Version

	Rand vmcontext.HeadChainRandomness

	// Lookback is the LookbackStateGetter; returns the state tree at a given epoch.
	Lookback vm.LookbackStateGetter

	// TipSetGetter returns the tipset key at any given epoch.
	TipSetGetter vm.TipSetGetter
}

// ExecuteMessage executes a conformance test vector message in a temporary LegacyVM.
func (d *Driver) ExecuteMessage(bs blockstoreutil.Blockstore, params ExecuteMessageParams) (*vm.Ret, cid.Cid, error) {
	if !d.vmFlush {
		// do not flush the LegacyVM, just the state tree; this should be used with
		// LOTUS_DISABLE_VM_BUF enabled, so writes will anyway be visible.
		_ = os.Setenv("LOTUS_DISABLE_VM_BUF", "iknowitsabadidea")
	}
	actorBuilder := register.DefaultActorBuilder
	register.GetDefaultActros()
	coderLoader := actorBuilder.Build()

	if params.Rand == nil {
		params.Rand = NewFixedRand()
	}
	if params.TipSetGetter == nil {
		// TODO: If/when we start writing conformance tests against the EVM, we'll need to
		// actually implement this and (unfortunately) capture any tipsets looked up by
		// messages.
		params.TipSetGetter = func(context.Context, abi.ChainEpoch) (types.TipSetKey, error) {
			return types.EmptyTSK, nil
		}
	}
	if params.Lookback == nil {
		// TODO: This lookback state returns the supplied precondition state tree, unconditionally.
		//  This is obviously not correct, but the lookback state tree is only used to validate the
		//  worker key when verifying a consensus fault. If the worker key hasn't changed in the
		//  current finality window, this workaround is enough.
		//  The correct solutions are documented in https://github.com/filecoin-project/ref-fvm/issues/381,
		//  but they're much harder to implement, and the tradeoffs aren't clear.
		params.Lookback = func(ctx context.Context, epoch abi.ChainEpoch) (*state.View, error) {
			cst := cbor.NewCborStore(bs)
			return state.NewView(cst, params.Preroot), nil
		}
	}

	mainNetParams := networks.Mainnet()
	node.SetNetParams(&mainNetParams.Network)
	ipldStore := cbor.NewCborStore(bs)
	chainDs := ds.NewMapDatastore() // just mock one
	// chainstore
	chainStore := chain.NewStore(chainDs, bs, cid.Undef, chainselector.Weight) // load genesis from car

	// chain fork
	chainFork, err := fork.NewChainFork(context.TODO(), chainStore, ipldStore, bs, &mainNetParams.Network, chainDs)
	faultChecker := consensusfault.NewFaultChecker(chainStore, chainFork)
	syscalls := vmsupport.NewSyscalls(faultChecker, impl.ProofVerifier)
	if err != nil {
		return nil, cid.Undef, err
	}
	var (
		ctx      = context.Background()
		vmOption = vm.VmOption{
			CircSupplyCalculator: func(ctx context.Context, epoch abi.ChainEpoch, tree tree.Tree) (abi.TokenAmount, error) {
				return params.CircSupply, nil
			},
			LookbackStateGetter: params.Lookback,
			NetworkVersion:      params.NetworkVersion,
			Rnd:                 params.Rand,
			BaseFee:             params.BaseFee,
			Fork:                chainFork,
			ActorCodeLoader:     &coderLoader,
			Epoch:               params.Epoch,
			Timestamp:           params.Timestamp,
			GasPriceSchedule:    gas.NewPricesSchedule(mainNetParams.Network.ForkUpgradeParam),
			PRoot:               params.Preroot,
			Bsstore:             bs,
			TipSetGetter:        params.TipSetGetter,
			SysCallsImpl:        syscalls,
		}
	)

	var vmi vm.Interface
	// register the chaos actor if required by the vector.
	if chaosOn, ok := d.selector["chaos_actor"]; ok && chaosOn == "true" {
		av, _ := actorstypes.VersionForNetwork(params.NetworkVersion)
		actorBuilder.AddMany(av, nil, builtin.MakeRegistryLegacy([]rtt.VMActor{chaos.Actor{}}))
		coderLoader = actorBuilder.Build()
		vmOption.ActorCodeLoader = &coderLoader
		vmi, err = vm.NewLegacyVM(ctx, vmOption)
		if err != nil {
			return nil, cid.Undef, err
		}
	} else {
		if params.NetworkVersion >= network.Version16 {
			vmi, err = fvm.NewFVM(ctx, &vmOption)
			if err != nil {
				return nil, cid.Undef, err
			}
		} else {
			vmi, err = vm.NewLegacyVM(ctx, vmOption)
			if err != nil {
				return nil, cid.Undef, err
			}
		}
	}

	ret, err := vmi.ApplyMessage(ctx, toChainMsg(params.Message))
	if err != nil {
		return nil, cid.Undef, err
	}

	var root cid.Cid
	if d.vmFlush {
		// flush the LegacyVM, committing the state tree changes and forcing a
		// recursive copy from the temporary blcokstore to the real blockstore.
		root, err = vmi.Flush(ctx)
		if err != nil {
			return nil, cid.Undef, err
		}
	} else {
		root, err = vmi.(vm.Interpreter).StateTree().Flush(d.ctx)
		if err != nil {
			return nil, cid.Undef, err
		}
	}

	return ret, root, err
}

// toChainMsg injects a synthetic 0-filled signature of the right length to
// messages that originate from secp256k senders, leaving all
// others untouched.
// TODO: generate a signature in the DSL so that it's encoded in
//
//	the test vector.
func toChainMsg(msg *types.Message) (ret types.ChainMsg) {
	ret = msg
	if msg.From.Protocol() == address.SECP256K1 {
		ret = &types.SignedMessage{
			Message: *msg,
			Signature: crypto.Signature{
				Type: crypto.SigTypeSecp256k1,
				Data: make([]byte, 65),
			},
		}
	}
	return ret
}

// BaseFeeOrDefault converts a basefee as passed in a test vector (go *big.Int
// type) to an abi.TokenAmount, or if nil it returns the DefaultBaseFee.
func BaseFeeOrDefault(basefee *gobig.Int) abi.TokenAmount {
	if basefee == nil {
		return DefaultBaseFee
	}
	return big.NewFromGo(basefee)
}

// CircSupplyOrDefault converts a circulating supply as passed in a test vector
// (go *big.Int type) to an abi.TokenAmount, or if nil it returns the
// DefaultCirculatingSupply.
func CircSupplyOrDefault(circSupply *gobig.Int) abi.TokenAmount {
	if circSupply == nil {
		return DefaultCirculatingSupply
	}
	return big.NewFromGo(circSupply)
}
