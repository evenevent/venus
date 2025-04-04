package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/libp2p/go-libp2p/core/peer"
	cbg "github.com/whyrusleeping/cbor-gen"

	actorstypes "github.com/filecoin-project/go-state-types/actors"
	market12 "github.com/filecoin-project/go-state-types/builtin/v12/market"
	market2 "github.com/filecoin-project/specs-actors/v2/actors/builtin/market"
	market5 "github.com/filecoin-project/specs-actors/v5/actors/builtin/market"
	"github.com/filecoin-project/venus/pkg/state/tree"
	"github.com/filecoin-project/venus/pkg/vm/register"
	"github.com/filecoin-project/venus/venus-shared/actors"
	"github.com/filecoin-project/venus/venus-shared/actors/adt"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin"
	_init "github.com/filecoin-project/venus/venus-shared/actors/builtin/init"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/market"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/miner"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/power"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/reward"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/verifreg"
	"github.com/filecoin-project/venus/venus-shared/actors/policy"
	v1api "github.com/filecoin-project/venus/venus-shared/api/chain/v1"
	"github.com/filecoin-project/venus/venus-shared/types"
	"github.com/filecoin-project/venus/venus-shared/utils"
)

var _ v1api.IMinerState = &minerStateAPI{}

type minerStateAPI struct {
	*ChainSubmodule
}

// NewMinerStateAPI create miner state api
func NewMinerStateAPI(chain *ChainSubmodule) v1api.IMinerState {
	return &minerStateAPI{ChainSubmodule: chain}
}

// StateMinerSectorAllocated checks if a sector is allocated
func (msa *minerStateAPI) StateMinerSectorAllocated(ctx context.Context, maddr address.Address, s abi.SectorNumber, tsk types.TipSetKey) (bool, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return false, fmt.Errorf("load Stmgr.ParentStateViewTsk(%s): %v", tsk, err)
	}
	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return false, fmt.Errorf("failed to load miner actor state: %v", err)
	}
	return mas.IsAllocated(s)
}

// StateSectorPreCommitInfo returns the PreCommit info for the specified miner's sector
func (msa *minerStateAPI) StateSectorPreCommitInfo(ctx context.Context, maddr address.Address, n abi.SectorNumber, tsk types.TipSetKey) (*types.SectorPreCommitOnChainInfo, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset:%s parent state view: %v", tsk, err)
	}

	return view.SectorPreCommitInfo(ctx, maddr, n)
}

// StateSectorGetInfo returns the on-chain info for the specified miner's sector. Returns null in case the sector info isn't found
// NOTE: returned info.Expiration may not be accurate in some cases, use StateSectorExpiration to get accurate
// expiration epoch
// return nil if sector not found
func (msa *minerStateAPI) StateSectorGetInfo(ctx context.Context, maddr address.Address, n abi.SectorNumber, tsk types.TipSetKey) (*miner.SectorOnChainInfo, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset %s: %v", tsk, err)
	}

	return view.MinerSectorInfo(ctx, maddr, n)
}

// StateSectorPartition finds deadline/partition with the specified sector
func (msa *minerStateAPI) StateSectorPartition(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tsk types.TipSetKey) (*miner.SectorLocation, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loadParentStateViewTsk(%s) failed:%v", tsk.String(), err)
	}

	return view.StateSectorPartition(ctx, maddr, sectorNumber)
}

// StateMinerSectorSize get miner sector size
func (msa *minerStateAPI) StateMinerSectorSize(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (abi.SectorSize, error) {
	// TODO: update storage-fsm to just StateMinerSectorAllocated
	mi, err := msa.StateMinerInfo(ctx, maddr, tsk)
	if err != nil {
		return 0, err
	}
	return mi.SectorSize, nil
}

// StateMinerInfo returns info about the indicated miner
func (msa *minerStateAPI) StateMinerInfo(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (types.MinerInfo, error) {
	ts, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return types.MinerInfo{}, fmt.Errorf("loading view %s: %v", tsk, err)
	}

	nv := msa.Fork.GetNetworkVersion(ctx, ts.Height())
	minfo, err := view.MinerInfo(ctx, maddr, nv)
	if err != nil {
		return types.MinerInfo{}, err
	}

	var pid *peer.ID
	if peerID, err := peer.IDFromBytes(minfo.PeerId); err == nil {
		pid = &peerID
	}

	ret := types.MinerInfo{
		Owner:                      minfo.Owner,
		Worker:                     minfo.Worker,
		ControlAddresses:           minfo.ControlAddresses,
		NewWorker:                  address.Undef,
		WorkerChangeEpoch:          -1,
		PeerId:                     pid,
		Multiaddrs:                 minfo.Multiaddrs,
		WindowPoStProofType:        minfo.WindowPoStProofType,
		SectorSize:                 minfo.SectorSize,
		WindowPoStPartitionSectors: minfo.WindowPoStPartitionSectors,
		ConsensusFaultElapsed:      minfo.ConsensusFaultElapsed,
		PendingOwnerAddress:        minfo.PendingOwnerAddress,
		Beneficiary:                minfo.Beneficiary,
		BeneficiaryTerm: &types.BeneficiaryTerm{
			Quota:      minfo.BeneficiaryTerm.Quota,
			UsedQuota:  minfo.BeneficiaryTerm.UsedQuota,
			Expiration: minfo.BeneficiaryTerm.Expiration,
		},
	}

	if minfo.PendingBeneficiaryTerm != nil {
		ret.PendingBeneficiaryTerm = &types.PendingBeneficiaryChange{
			NewBeneficiary:        minfo.PendingBeneficiaryTerm.NewBeneficiary,
			NewQuota:              minfo.PendingBeneficiaryTerm.NewQuota,
			NewExpiration:         minfo.PendingBeneficiaryTerm.NewExpiration,
			ApprovedByBeneficiary: minfo.PendingBeneficiaryTerm.ApprovedByBeneficiary,
			ApprovedByNominee:     minfo.PendingBeneficiaryTerm.ApprovedByNominee,
		}
	}

	if minfo.PendingWorkerKey != nil {
		ret.NewWorker = minfo.PendingWorkerKey.NewWorker
		ret.WorkerChangeEpoch = minfo.PendingWorkerKey.EffectiveAt
	}

	return ret, nil
}

// StateMinerWorkerAddress get miner worker address
func (msa *minerStateAPI) StateMinerWorkerAddress(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (address.Address, error) {
	// TODO: update storage-fsm to just StateMinerInfo
	mi, err := msa.StateMinerInfo(ctx, maddr, tsk)
	if err != nil {
		return address.Undef, err
	}
	return mi.Worker, nil
}

// StateMinerRecoveries returns a bitfield indicating the recovering sectors of the given miner
func (msa *minerStateAPI) StateMinerRecoveries(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (bitfield.BitField, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("loading view %s: %v", tsk, err)
	}

	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	return miner.AllPartSectors(mas, miner.Partition.RecoveringSectors)
}

// StateMinerFaults returns a bitfield indicating the faulty sectors of the given miner
func (msa *minerStateAPI) StateMinerFaults(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (bitfield.BitField, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	return miner.AllPartSectors(mas, miner.Partition.FaultySectors)
}

func (msa *minerStateAPI) StateAllMinerFaults(ctx context.Context, lookback abi.ChainEpoch, endTsk types.TipSetKey) ([]*types.Fault, error) {
	return nil, fmt.Errorf("fixme")
}

// StateMinerProvingDeadline calculates the deadline at some epoch for a proving period
// and returns the deadline-related calculations.
func (msa *minerStateAPI) StateMinerProvingDeadline(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (*dline.Info, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("GetTipset failed:%v", err)
	}

	_, view, err := msa.Stmgr.ParentStateView(ctx, ts)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}
	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return nil, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	di, err := mas.DeadlineInfo(ts.Height())
	if err != nil {
		return nil, fmt.Errorf("failed to get deadline info: %v", err)
	}

	return di.NextNotElapsed(), nil
}

// StateMinerPartitions returns all partitions in the specified deadline
func (msa *minerStateAPI) StateMinerPartitions(ctx context.Context, maddr address.Address, dlIdx uint64, tsk types.TipSetKey) ([]types.Partition, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return nil, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	dl, err := mas.LoadDeadline(dlIdx)
	if err != nil {
		return nil, fmt.Errorf("failed to load the deadline: %v", err)
	}

	var out []types.Partition
	err = dl.ForEachPartition(func(_ uint64, part miner.Partition) error {
		allSectors, err := part.AllSectors()
		if err != nil {
			return fmt.Errorf("getting AllSectors: %v", err)
		}

		faultySectors, err := part.FaultySectors()
		if err != nil {
			return fmt.Errorf("getting FaultySectors: %v", err)
		}

		recoveringSectors, err := part.RecoveringSectors()
		if err != nil {
			return fmt.Errorf("getting RecoveringSectors: %v", err)
		}

		liveSectors, err := part.LiveSectors()
		if err != nil {
			return fmt.Errorf("getting LiveSectors: %v", err)
		}

		activeSectors, err := part.ActiveSectors()
		if err != nil {
			return fmt.Errorf("getting ActiveSectors: %v", err)
		}

		out = append(out, types.Partition{
			AllSectors:        allSectors,
			FaultySectors:     faultySectors,
			RecoveringSectors: recoveringSectors,
			LiveSectors:       liveSectors,
			ActiveSectors:     activeSectors,
		})
		return nil
	})

	return out, err
}

// StateMinerDeadlines returns all the proving deadlines for the given miner
func (msa *minerStateAPI) StateMinerDeadlines(ctx context.Context, maddr address.Address, tsk types.TipSetKey) ([]types.Deadline, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return nil, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	deadlines, err := mas.NumDeadlines()
	if err != nil {
		return nil, fmt.Errorf("getting deadline count: %v", err)
	}

	out := make([]types.Deadline, deadlines)
	if err := mas.ForEachDeadline(func(i uint64, dl miner.Deadline) error {
		ps, err := dl.PartitionsPoSted()
		if err != nil {
			return err
		}

		l, err := dl.DisputableProofCount()
		if err != nil {
			return err
		}
		dailyFee, err := dl.DailyFee()
		if err != nil {
			return err
		}

		out[i] = types.Deadline{
			PostSubmissions:      ps,
			DisputableProofCount: l,
			DailyFee:             dailyFee,
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// StateMinerSectors returns info about the given miner's sectors. If the filter bitfield is nil, all sectors are included.
func (msa *minerStateAPI) StateMinerSectors(ctx context.Context, maddr address.Address, sectorNos *bitfield.BitField, tsk types.TipSetKey) ([]*miner.SectorOnChainInfo, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMinerState(ctx, maddr)
	if err != nil {
		return nil, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	return mas.LoadSectors(sectorNos)
}

// StateMarketStorageDeal returns information about the indicated deal
func (msa *minerStateAPI) StateMarketStorageDeal(ctx context.Context, dealID abi.DealID, tsk types.TipSetKey) (*types.MarketDeal, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMarketState(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	proposals, err := mas.Proposals()
	if err != nil {
		return nil, err
	}

	proposal, found, err := proposals.Get(dealID)

	if err != nil {
		return nil, err
	} else if !found {
		return nil, fmt.Errorf("deal %d not found", dealID)
	}

	states, err := mas.States()
	if err != nil {
		return nil, err
	}

	st, found, err := states.Get(dealID)
	if err != nil {
		return nil, err
	}

	if !found {
		st = market.EmptyDealState()
	}

	return &types.MarketDeal{
		Proposal: *proposal,
		State:    types.MakeDealState(st),
	}, nil
}

func (msa *minerStateAPI) StateGetAllocationIdForPendingDeal(ctx context.Context, dealID abi.DealID, tsk types.TipSetKey) (verifreg.AllocationId, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return verifreg.NoAllocationID, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMarketState(ctx)
	if err != nil {
		return verifreg.NoAllocationID, fmt.Errorf("failed to load miner actor state: %v", err)
	}

	allocationID, err := mas.GetAllocationIdForPendingDeal(dealID)
	if err != nil {
		return verifreg.NoAllocationID, err
	}

	return allocationID, nil
}

// StateGetAllocationForPendingDeal returns the allocation for a given deal ID of a pending deal. Returns nil if
// pending allocation is not found.
func (msa *minerStateAPI) StateGetAllocationForPendingDeal(ctx context.Context, dealID abi.DealID, tsk types.TipSetKey) (*types.Allocation, error) {
	allocationID, err := msa.StateGetAllocationIdForPendingDeal(ctx, dealID, tsk)
	if err != nil {
		return nil, err
	}

	if allocationID == types.NoAllocationID {
		return nil, nil
	}

	dealState, err := msa.StateMarketStorageDeal(ctx, dealID, tsk)
	if err != nil {
		return nil, err
	}

	return msa.StateGetAllocation(ctx, dealState.Proposal.Client, allocationID, tsk)
}

// StateGetAllocation returns the allocation for a given address and allocation ID.
func (msa *minerStateAPI) StateGetAllocation(ctx context.Context, clientAddr address.Address, allocationID types.AllocationId, tsk types.TipSetKey) (*types.Allocation, error) {
	idAddr, err := msa.ChainSubmodule.API().StateLookupID(ctx, clientAddr, tsk)
	if err != nil {
		return nil, err
	}

	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	allocation, found, err := st.GetAllocation(idAddr, allocationID)
	if err != nil {
		return nil, fmt.Errorf("getting allocation: %w", err)
	}
	if !found {
		return nil, nil
	}

	return allocation, nil
}

func (msa *minerStateAPI) StateGetAllAllocations(ctx context.Context, tsk types.TipSetKey) (map[types.AllocationId]types.Allocation, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	allocations, err := st.GetAllAllocations()
	if err != nil {
		return nil, fmt.Errorf("getting all allocations: %w", err)
	}

	return allocations, nil
}

// StateGetAllocations returns the all the allocations for a given client.
func (msa *minerStateAPI) StateGetAllocations(ctx context.Context, clientAddr address.Address, tsk types.TipSetKey) (map[types.AllocationId]types.Allocation, error) {
	idAddr, err := msa.ChainSubmodule.API().StateLookupID(ctx, clientAddr, tsk)
	if err != nil {
		return nil, err
	}

	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	allocations, err := st.GetAllocations(idAddr)
	if err != nil {
		return nil, fmt.Errorf("getting allocations: %w", err)
	}

	return allocations, nil
}

// StateGetClaim returns the claim for a given address and claim ID.
func (msa *minerStateAPI) StateGetClaim(ctx context.Context, providerAddr address.Address, claimID types.ClaimId, tsk types.TipSetKey) (*types.Claim, error) {
	idAddr, err := msa.ChainSubmodule.API().StateLookupID(ctx, providerAddr, tsk)
	if err != nil {
		return nil, err
	}

	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	claim, found, err := st.GetClaim(idAddr, claimID)
	if err != nil {
		return nil, fmt.Errorf("getting claim: %w", err)
	}
	if !found {
		return nil, nil
	}

	return claim, nil
}

// StateGetClaims returns the all the claims for a given provider.
func (msa *minerStateAPI) StateGetClaims(ctx context.Context, providerAddr address.Address, tsk types.TipSetKey) (map[types.ClaimId]types.Claim, error) {
	idAddr, err := msa.ChainSubmodule.API().StateLookupID(ctx, providerAddr, tsk)
	if err != nil {
		return nil, err
	}

	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	claims, err := st.GetClaims(idAddr)
	if err != nil {
		return nil, fmt.Errorf("getting claims: %w", err)
	}

	return claims, nil
}

func (msa *minerStateAPI) StateGetAllClaims(ctx context.Context, tsk types.TipSetKey) (map[verifreg.ClaimId]verifreg.Claim, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	st, err := view.LoadVerifregActor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load verifreg actor state: %v", err)
	}

	claims, err := st.GetAllClaims()
	if err != nil {
		return nil, fmt.Errorf("getting all claims: %w", err)
	}

	return claims, nil
}

// StateComputeDataCID computes DataCID from a set of on-chain deals
func (msa *minerStateAPI) StateComputeDataCID(ctx context.Context, maddr address.Address, sectorType abi.RegisteredSealProof, deals []abi.DealID, tsk types.TipSetKey) (cid.Cid, error) {
	nv, err := msa.API().StateNetworkVersion(ctx, tsk)
	if err != nil {
		return cid.Cid{}, err
	}

	if nv < network.Version13 {
		return msa.stateComputeDataCIDv1(ctx, maddr, sectorType, deals, tsk)
	} else if nv < network.Version21 {
		return msa.stateComputeDataCIDv2(ctx, maddr, sectorType, deals, tsk)
	} else {
		return msa.stateComputeDataCIDv3(ctx, maddr, sectorType, deals, tsk)
	}
}

func (msa *minerStateAPI) stateComputeDataCIDv1(ctx context.Context, maddr address.Address, sectorType abi.RegisteredSealProof, deals []abi.DealID, tsk types.TipSetKey) (cid.Cid, error) {
	ccparams, actorErr := actors.SerializeParams(&market2.ComputeDataCommitmentParams{
		DealIDs:    deals,
		SectorType: sectorType,
	})
	if actorErr != nil {
		return cid.Undef, fmt.Errorf("computing params for ComputeDataCommitment: %v", actorErr)
	}

	ccmt := &types.Message{
		To:    market.Address,
		From:  maddr,
		Value: types.NewInt(0),
		// Hard coded, because the method has since been deprecated
		Method: 8,
		Params: ccparams,
	}
	r, err := msa.API().StateCall(ctx, ccmt, tsk)
	if err != nil {
		return cid.Undef, fmt.Errorf("calling ComputeDataCommitment: %w", err)
	}
	if r.MsgRct.ExitCode != 0 {
		return cid.Undef, fmt.Errorf("receipt for ComputeDataCommitment had exit code %d", r.MsgRct.ExitCode)
	}

	var c cbg.CborCid
	if err := c.UnmarshalCBOR(bytes.NewReader(r.MsgRct.Return)); err != nil {
		return cid.Undef, fmt.Errorf("failed to unmarshal CBOR to CborCid: %w", err)
	}

	return cid.Cid(c), nil
}

func (msa *minerStateAPI) stateComputeDataCIDv2(ctx context.Context, maddr address.Address, sectorType abi.RegisteredSealProof, deals []abi.DealID, tsk types.TipSetKey) (cid.Cid, error) {
	var err error
	ccparams, err := actors.SerializeParams(&market5.ComputeDataCommitmentParams{
		Inputs: []*market5.SectorDataSpec{
			{
				DealIDs:    deals,
				SectorType: sectorType,
			},
		},
	})

	if err != nil {
		return cid.Undef, fmt.Errorf("computing params for ComputeDataCommitment: %w", err)
	}
	ccmt := &types.Message{
		To:    market.Address,
		From:  maddr,
		Value: types.NewInt(0),
		// Hard coded, because the method has since been deprecated
		Method: 8,
		Params: ccparams,
	}
	r, err := msa.API().StateCall(ctx, ccmt, tsk)
	if err != nil {
		return cid.Undef, fmt.Errorf("calling ComputeDataCommitment: %w", err)
	}
	if r.MsgRct.ExitCode != 0 {
		return cid.Undef, fmt.Errorf("receipt for ComputeDataCommitment had exit code %d", r.MsgRct.ExitCode)
	}

	var cr market5.ComputeDataCommitmentReturn
	if err := cr.UnmarshalCBOR(bytes.NewReader(r.MsgRct.Return)); err != nil {
		return cid.Undef, fmt.Errorf("failed to unmarshal CBOR to CborCid: %w", err)
	}
	if len(cr.CommDs) != 1 {
		return cid.Undef, fmt.Errorf("CommD output must have 1 entry")
	}
	return cid.Cid(cr.CommDs[0]), nil
}

func (msa *minerStateAPI) stateComputeDataCIDv3(ctx context.Context, maddr address.Address, sectorType abi.RegisteredSealProof, deals []abi.DealID, tsk types.TipSetKey) (cid.Cid, error) {
	if len(deals) == 0 {
		return cid.Undef, nil
	}

	var err error
	ccparams, err := actors.SerializeParams(&market12.VerifyDealsForActivationParams{
		Sectors: []market12.SectorDeals{{
			SectorType:   sectorType,
			SectorExpiry: math.MaxInt64,
			DealIDs:      deals,
		}},
	})

	if err != nil {
		return cid.Undef, fmt.Errorf("computing params for VerifyDealsForActivation: %w", err)
	}
	ccmt := &types.Message{
		To:     market.Address,
		From:   maddr,
		Value:  types.NewInt(0),
		Method: market.Methods.VerifyDealsForActivation,
		Params: ccparams,
	}
	r, err := msa.API().StateCall(ctx, ccmt, tsk)
	if err != nil {
		return cid.Undef, fmt.Errorf("calling VerifyDealsForActivation: %w", err)
	}
	if r.MsgRct.ExitCode != 0 {
		return cid.Undef, fmt.Errorf("receipt for VerifyDealsForActivation had exit code %d", r.MsgRct.ExitCode)
	}

	var cr market12.VerifyDealsForActivationReturn
	if err := cr.UnmarshalCBOR(bytes.NewReader(r.MsgRct.Return)); err != nil {
		return cid.Undef, fmt.Errorf("failed to unmarshal CBOR to VerifyDealsForActivationReturn: %w", err)
	}
	if len(cr.UnsealedCIDs) != 1 {
		return cid.Undef, fmt.Errorf("sectors output must have 1 entry")
	}
	ucid := cr.UnsealedCIDs[0]
	if ucid == nil {
		return cid.Undef, fmt.Errorf("computed data CID is nil")
	}
	return *ucid, nil
}

var (
	initialPledgeNum = big.NewInt(110)
	initialPledgeDen = big.NewInt(100)
)

func (msa *minerStateAPI) calculateSectorWeight(ctx context.Context, maddr address.Address, pci miner.SectorPreCommitInfo, height abi.ChainEpoch, state *tree.State) (abi.StoragePower, error) {
	ssize, err := pci.SealProof.SectorSize()
	if err != nil {
		return types.EmptyInt, fmt.Errorf("failed to resolve sector size for seal proof: %w", err)
	}

	store := msa.ChainReader.Store(ctx)

	var sectorWeight abi.StoragePower
	if act, found, err := state.GetActor(ctx, market.Address); err != nil {
		return types.EmptyInt, fmt.Errorf("loading market actor: %w", err)
	} else if !found {
		return types.EmptyInt, fmt.Errorf("market actor not found")
	} else if s, err := market.Load(store, act); err != nil {
		return types.EmptyInt, fmt.Errorf("loading market actor state: %w", err)
	} else if vw, err := s.VerifyDealsForActivation(maddr, pci.DealIDs, height, pci.Expiration); err != nil {
		return types.EmptyInt, fmt.Errorf("verifying deals for activation: %w", err)
	} else {
		// NB: not exactly accurate, but should always lead us to *over* estimate, not under
		duration := pci.Expiration - height
		sectorWeight = builtin.QAPowerForWeight(ssize, duration, vw)
	}

	return sectorWeight, nil
}

func (msa *minerStateAPI) pledgeCalculationInputs(ctx context.Context, state tree.Tree) (abi.TokenAmount, *builtin.FilterEstimate, error) {
	store := msa.ChainReader.Store(ctx)

	var (
		powerSmoothed    builtin.FilterEstimate
		pledgeCollateral abi.TokenAmount
	)
	if act, found, err := state.GetActor(ctx, power.Address); err != nil {
		return types.EmptyInt, nil, fmt.Errorf("loading power actor: %w", err)
	} else if !found {
		return types.EmptyInt, nil, fmt.Errorf("power actor not found")
	} else if s, err := power.Load(store, act); err != nil {
		return types.EmptyInt, nil, fmt.Errorf("loading power actor state: %w", err)
	} else if p, err := s.TotalPowerSmoothed(); err != nil {
		return types.EmptyInt, nil, fmt.Errorf("failed to determine total power: %w", err)
	} else if c, err := s.TotalLocked(); err != nil {
		return types.EmptyInt, nil, fmt.Errorf("failed to determine pledge collateral: %w", err)
	} else {
		powerSmoothed = p
		pledgeCollateral = c
	}

	return pledgeCollateral, &powerSmoothed, nil
}

// StateMinerPreCommitDepositForPower returns the precommit deposit for the specified miner's sector
func (msa *minerStateAPI) StateMinerPreCommitDepositForPower(ctx context.Context, maddr address.Address, pci types.SectorPreCommitInfo, tsk types.TipSetKey) (big.Int, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return big.Int{}, err
	}

	var sTree *tree.State
	_, sTree, err = msa.Stmgr.ParentState(ctx, ts)
	if err != nil {
		return big.Int{}, fmt.Errorf("ParentState failed:%v", err)
	}

	rewardActor, found, err := sTree.GetActor(ctx, reward.Address)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor: %w", err)
	}
	if !found {
		return types.EmptyInt, fmt.Errorf("reward actor not found")
	}

	rewardState, err := reward.Load(msa.ChainReader.Store(ctx), rewardActor)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor state: %w", err)
	}

	var sectorWeight abi.StoragePower
	if msa.ChainSubmodule.Fork.GetNetworkVersion(ctx, ts.Height()) <= network.Version16 {
		if sectorWeight, err = msa.calculateSectorWeight(ctx, maddr, pci, ts.Height(), sTree); err != nil {
			return types.EmptyInt, err
		}
	} else {
		ssize, err := pci.SealProof.SectorSize()
		if err != nil {
			return types.EmptyInt, fmt.Errorf("failed to resolve sector size for seal proof: %w", err)
		}
		sectorWeight = miner.QAPowerMax(ssize)
	}

	_, powerSmoothed, err := msa.pledgeCalculationInputs(ctx, sTree)
	if err != nil {
		return types.EmptyInt, err
	}

	deposit, err := rewardState.PreCommitDepositForPower(*powerSmoothed, sectorWeight)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("calculating precommit deposit: %w", err)
	}

	return types.BigDiv(types.BigMul(deposit, initialPledgeNum), initialPledgeDen), nil
}

// StateMinerInitialPledgeCollateral returns the initial pledge collateral for the specified miner's sector
func (msa *minerStateAPI) StateMinerInitialPledgeCollateral(ctx context.Context, maddr address.Address, pci types.SectorPreCommitInfo, tsk types.TipSetKey) (big.Int, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return big.Int{}, fmt.Errorf("loading tipset %s: %v", tsk, err)
	}

	_, state, err := msa.Stmgr.ParentState(ctx, ts)
	if err != nil {
		return big.Int{}, fmt.Errorf("loading tipset(%s) parent state failed: %v", tsk, err)
	}

	rewardActor, found, err := state.GetActor(ctx, reward.Address)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor: %w", err)
	}
	if !found {
		return types.EmptyInt, fmt.Errorf("reward actor not found")
	}

	rewardState, err := reward.Load(msa.ChainReader.Store(ctx), rewardActor)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor state: %w", err)
	}

	sectorWeight, err := msa.calculateSectorWeight(ctx, maddr, pci, ts.Height(), state)
	if err != nil {
		return types.EmptyInt, err
	}

	pledgeCollateral, powerSmoothed, err := msa.pledgeCalculationInputs(ctx, state)
	if err != nil {
		return types.EmptyInt, err
	}

	circSupply, err := msa.StateVMCirculatingSupplyInternal(ctx, ts.Key())
	if err != nil {
		return types.EmptyInt, fmt.Errorf("getting circulating supply: %w", err)
	}

	epochsSinceRampStart, rampDurationEpochs, err := msa.getPledgeRampParams(ctx, ts.Height(), state)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("getting pledge ramp params: %w", err)
	}

	initialPledge, err := rewardState.InitialPledgeForPower(
		sectorWeight,
		pledgeCollateral,
		powerSmoothed,
		circSupply.FilCirculating,
		epochsSinceRampStart,
		rampDurationEpochs,
	)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("calculating initial pledge: %w", err)
	}

	return types.BigDiv(types.BigMul(initialPledge, initialPledgeNum), initialPledgeDen), nil
}

// getPledgeRampParams returns epochsSinceRampStart, rampDurationEpochs, or 0, 0 if the pledge ramp is not active.
func (msa *minerStateAPI) getPledgeRampParams(ctx context.Context, height abi.ChainEpoch, state tree.Tree) (int64, uint64, error) {
	if powerActor, found, err := state.GetActor(ctx, power.Address); err != nil {
		return 0, 0, fmt.Errorf("loading power actor: %w", err)
	} else if !found {
		return 0, 0, fmt.Errorf("power actor not found")
	} else if powerState, err := power.Load(msa.ChainReader.Store(ctx), powerActor); err != nil {
		return 0, 0, fmt.Errorf("loading power actor state: %w", err)
	} else if powerState.RampStartEpoch() > 0 {
		return int64(height) - powerState.RampStartEpoch(), powerState.RampDurationEpochs(), nil
	}
	return 0, 0, nil
}

func (msa *minerStateAPI) StateMinerInitialPledgeForSector(ctx context.Context, sectorDuration abi.ChainEpoch, sectorSize abi.SectorSize, verifiedSize uint64, tsk types.TipSetKey) (types.BigInt, error) {
	if sectorDuration <= 0 {
		return types.EmptyInt, fmt.Errorf("sector duration must greater than 0")
	}
	if sectorSize == 0 {
		return types.EmptyInt, fmt.Errorf("sector size must be non-zero")
	}
	if verifiedSize > uint64(sectorSize) {
		return types.EmptyInt, fmt.Errorf("verified size must be less than or equal to sector size")
	}

	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading tipset %s: %w", tsk, err)
	}

	_, state, err := msa.Stmgr.ParentState(ctx, ts)
	if err != nil {
		return big.Int{}, fmt.Errorf("loading tipset(%s) parent state failed: %v", tsk, err)
	}

	rewardActor, found, err := state.GetActor(ctx, reward.Address)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor: %w", err)
	}
	if !found {
		return types.EmptyInt, fmt.Errorf("reward actor not found")
	}

	rewardState, err := reward.Load(msa.ChainReader.Store(ctx), rewardActor)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("loading reward actor state: %w", err)
	}

	circSupply, err := msa.StateVMCirculatingSupplyInternal(ctx, ts.Key())
	if err != nil {
		return types.EmptyInt, fmt.Errorf("getting circulating supply: %w", err)
	}

	pledgeCollateral, powerSmoothed, err := msa.pledgeCalculationInputs(ctx, state)
	if err != nil {
		return types.EmptyInt, err
	}

	verifiedWeight := big.Mul(big.NewIntUnsigned(verifiedSize), big.NewInt(int64(sectorDuration)))
	sectorWeight := builtin.QAPowerForWeight(sectorSize, sectorDuration, verifiedWeight)

	epochsSinceRampStart, rampDurationEpochs, err := msa.getPledgeRampParams(ctx, ts.Height(), state)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("getting pledge ramp params: %w", err)
	}

	initialPledge, err := rewardState.InitialPledgeForPower(
		sectorWeight,
		pledgeCollateral,
		powerSmoothed,
		circSupply.FilCirculating,
		epochsSinceRampStart,
		rampDurationEpochs,
	)
	if err != nil {
		return types.EmptyInt, fmt.Errorf("calculating initial pledge: %w", err)
	}

	return types.BigDiv(types.BigMul(initialPledge, initialPledgeNum), initialPledgeDen), nil
}

// StateVMCirculatingSupplyInternal returns an approximation of the circulating supply of Filecoin at the given tipset.
// This is the value reported by the runtime interface to actors code.
func (msa *minerStateAPI) StateVMCirculatingSupplyInternal(ctx context.Context, tsk types.TipSetKey) (types.CirculatingSupply, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return types.CirculatingSupply{}, err
	}

	_, sTree, err := msa.Stmgr.ParentState(ctx, ts)
	if err != nil {
		return types.CirculatingSupply{}, err
	}

	return msa.CirculatingSupplyCalculator.GetCirculatingSupplyDetailed(ctx, ts.Height(), sTree)
}

// StateCirculatingSupply returns the exact circulating supply of Filecoin at the given tipset.
// This is not used anywhere in the protocol itself, and is only for external consumption.
func (msa *minerStateAPI) StateCirculatingSupply(ctx context.Context, tsk types.TipSetKey) (abi.TokenAmount, error) {
	// stmgr.ParentStateTsk make sure the parent state specified by 'tsk' exists
	parent, _, err := msa.Stmgr.ParentStateTsk(ctx, tsk)
	if err != nil {
		return abi.TokenAmount{}, fmt.Errorf("tipset(%s) parent state failed:%v",
			tsk.String(), err)
	}

	ts, err := msa.ChainReader.GetTipSet(ctx, parent.Key())
	if err != nil {
		return abi.TokenAmount{}, err
	}

	root, err := msa.ChainReader.GetTipSetStateRoot(ctx, ts)
	if err != nil {
		return abi.TokenAmount{}, err
	}

	sTree, err := tree.LoadState(ctx, msa.ChainReader.StateStore(), root)
	if err != nil {
		return abi.TokenAmount{}, err
	}

	return msa.CirculatingSupplyCalculator.GetCirculatingSupply(ctx, ts.Height(), sTree)
}

// StateMarketDeals returns information about every deal in the Storage Market
func (msa *minerStateAPI) StateMarketDeals(ctx context.Context, tsk types.TipSetKey) (map[string]*types.MarketDeal, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%w", err)
	}
	return view.StateMarketDeals(ctx, tsk)
}

// StateMinerActiveSectors returns info about sectors that a given miner is actively proving.
func (msa *minerStateAPI) StateMinerActiveSectors(ctx context.Context, maddr address.Address, tsk types.TipSetKey) ([]*miner.SectorOnChainInfo, error) { // TODO: only used in cli
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}
	return view.StateMinerActiveSectors(ctx, maddr, tsk)
}

// StateLookupID retrieves the ID address of the given address
func (msa *minerStateAPI) StateLookupID(ctx context.Context, addr address.Address, tsk types.TipSetKey) (address.Address, error) {
	_, state, err := msa.Stmgr.ParentStateTsk(ctx, tsk)
	if err != nil {
		return address.Undef, fmt.Errorf("load state failed: %v", err)
	}

	return state.LookupID(addr)
}

func (msa *minerStateAPI) StateLookupRobustAddress(ctx context.Context, idAddr address.Address, tsk types.TipSetKey) (address.Address, error) {
	idAddrDecoded, err := address.IDFromAddress(idAddr)
	if err != nil {
		return address.Undef, fmt.Errorf("failed to decode provided address as id addr: %w", err)
	}

	cst := cbornode.NewCborStore(msa.ChainReader.Blockstore())
	wrapStore := adt.WrapStore(ctx, cst)

	_, state, err := msa.Stmgr.ParentStateTsk(ctx, tsk)
	if err != nil {
		return address.Undef, fmt.Errorf("load state failed: %w", err)
	}

	initActor, found, err := state.GetActor(ctx, _init.Address)
	if err != nil {
		return address.Undef, fmt.Errorf("load init actor: %w", err)
	}
	if !found {
		return address.Undef, fmt.Errorf("not found actor: %w", err)
	}

	initState, err := _init.Load(wrapStore, initActor)
	if err != nil {
		return address.Undef, fmt.Errorf("load init state: %w", err)
	}
	robustAddr := address.Undef

	err = initState.ForEachActor(func(id abi.ActorID, addr address.Address) error {
		if uint64(id) == idAddrDecoded {
			robustAddr = addr
			// Hacky way to early return from ForEach
			return errors.New("robust address found")
		}
		return nil
	})
	if robustAddr == address.Undef {
		if err == nil {
			return address.Undef, fmt.Errorf("address %s not found", idAddr.String())
		}
		return address.Undef, fmt.Errorf("finding address: %w", err)
	}
	return robustAddr, nil
}

// StateListMiners returns the addresses of every miner that has claimed power in the Power Actor
func (msa *minerStateAPI) StateListMiners(ctx context.Context, tsk types.TipSetKey) ([]address.Address, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	return view.StateListMiners(ctx, tsk)
}

// StateListActors returns the addresses of every actor in the state
func (msa *minerStateAPI) StateListActors(ctx context.Context, tsk types.TipSetKey) ([]address.Address, error) {
	_, stat, err := msa.Stmgr.TipsetStateTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("load tipset state from key:%s failed:%v",
			tsk.String(), err)
	}
	var out []address.Address
	err = stat.ForEach(func(addr tree.ActorKey, act *types.Actor) error {
		out = append(out, addr)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// StateMinerPower returns the power of the indicated miner
func (msa *minerStateAPI) StateMinerPower(ctx context.Context, addr address.Address, tsk types.TipSetKey) (*types.MinerPower, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}
	mp, net, hmp, err := view.StateMinerPower(ctx, addr, tsk)
	if err != nil {
		return nil, err
	}

	return &types.MinerPower{
		MinerPower:  mp,
		TotalPower:  net,
		HasMinPower: hmp,
	}, nil
}

// StateMinerAvailableBalance returns the portion of a miner's balance that can be withdrawn or spent
func (msa *minerStateAPI) StateMinerAvailableBalance(ctx context.Context, maddr address.Address, tsk types.TipSetKey) (big.Int, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return big.Int{}, fmt.Errorf("failed to get tipset for %s, %v", tsk.String(), err)
	}
	_, view, err := msa.Stmgr.ParentStateView(ctx, ts)
	if err != nil {
		return big.Int{}, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	return view.StateMinerAvailableBalance(ctx, maddr, ts)
}

// StateSectorExpiration returns epoch at which given sector will expire
func (msa *minerStateAPI) StateSectorExpiration(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tsk types.TipSetKey) (*miner.SectorExpiration, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	return view.StateSectorExpiration(ctx, maddr, sectorNumber, tsk)
}

// StateMinerSectorCount returns the number of sectors in a miner's sector set and proving set
func (msa *minerStateAPI) StateMinerSectorCount(ctx context.Context, addr address.Address, tsk types.TipSetKey) (types.MinerSectors, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return types.MinerSectors{}, fmt.Errorf("Stmgr.ParentStateViewTsk failed:%v", err)
	}

	mas, err := view.LoadMinerState(ctx, addr)
	if err != nil {
		return types.MinerSectors{}, err
	}

	var activeCount, liveCount, faultyCount uint64
	if err := mas.ForEachDeadline(func(_ uint64, dl miner.Deadline) error {
		return dl.ForEachPartition(func(_ uint64, part miner.Partition) error {
			if active, err := part.ActiveSectors(); err != nil {
				return err
			} else if count, err := active.Count(); err != nil {
				return err
			} else {
				activeCount += count
			}
			if live, err := part.LiveSectors(); err != nil {
				return err
			} else if count, err := live.Count(); err != nil {
				return err
			} else {
				liveCount += count
			}
			if faulty, err := part.FaultySectors(); err != nil {
				return err
			} else if count, err := faulty.Count(); err != nil {
				return err
			} else {
				faultyCount += count
			}
			return nil
		})
	}); err != nil {
		return types.MinerSectors{}, err
	}
	return types.MinerSectors{Live: liveCount, Active: activeCount, Faulty: faultyCount}, nil
}

// StateMarketBalance looks up the Escrow and Locked balances of the given address in the Storage Market
func (msa *minerStateAPI) StateMarketBalance(ctx context.Context, addr address.Address, tsk types.TipSetKey) (types.MarketBalance, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return types.MarketBalanceNil, fmt.Errorf("loading view %s: %v", tsk, err)
	}

	mstate, err := view.LoadMarketState(ctx)
	if err != nil {
		return types.MarketBalanceNil, err
	}

	addr, err = view.LookupID(ctx, addr)
	if err != nil {
		return types.MarketBalanceNil, err
	}

	var out types.MarketBalance

	et, err := mstate.EscrowTable()
	if err != nil {
		return types.MarketBalanceNil, err
	}
	out.Escrow, err = et.Get(addr)
	if err != nil {
		return types.MarketBalanceNil, fmt.Errorf("getting escrow balance: %v", err)
	}

	lt, err := mstate.LockedTable()
	if err != nil {
		return types.MarketBalanceNil, err
	}
	out.Locked, err = lt.Get(addr)
	if err != nil {
		return types.MarketBalanceNil, fmt.Errorf("getting locked balance: %v", err)
	}

	return out, nil
}

var (
	dealProviderCollateralNum = types.NewInt(110)
	dealProviderCollateralDen = types.NewInt(100)
)

// StateDealProviderCollateralBounds returns the min and max collateral a storage provider
// can issue. It takes the deal size and verified status as parameters.
func (msa *minerStateAPI) StateDealProviderCollateralBounds(ctx context.Context, size abi.PaddedPieceSize, verified bool, tsk types.TipSetKey) (types.DealCollateralBounds, error) {
	ts, _, view, err := msa.Stmgr.StateViewTsk(ctx, tsk)
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("loading state view %s: %v", tsk, err)
	}

	pst, err := view.LoadPowerState(ctx)
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("failed to load power actor state: %v", err)
	}

	rst, err := view.LoadRewardState(ctx)
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("failed to load reward actor state: %v", err)
	}

	circ, err := msa.StateVMCirculatingSupplyInternal(ctx, ts.Key())
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("getting total circulating supply: %v", err)
	}

	powClaim, err := pst.TotalPower()
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("getting total power: %v", err)
	}

	rewPow, err := rst.ThisEpochBaselinePower()
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("getting reward baseline power: %v", err)
	}

	min, max, err := policy.DealProviderCollateralBounds(size,
		verified,
		powClaim.RawBytePower,
		powClaim.QualityAdjPower,
		rewPow,
		circ.FilCirculating,
		msa.Fork.GetNetworkVersion(ctx, ts.Height()))
	if err != nil {
		return types.DealCollateralBounds{}, fmt.Errorf("getting deal provider coll bounds: %v", err)
	}
	return types.DealCollateralBounds{
		Min: types.BigDiv(types.BigMul(min, dealProviderCollateralNum), dealProviderCollateralDen),
		Max: max,
	}, nil
}

// StateVerifiedClientStatus returns the data cap for the given address.
// Returns zero if there is no entry in the data cap table for the
// address.
func (msa *minerStateAPI) StateVerifiedClientStatus(ctx context.Context, addr address.Address, tsk types.TipSetKey) (*abi.StoragePower, error) {
	_, _, view, err := msa.Stmgr.StateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading state view %s: %v", tsk, err)
	}

	aid, err := view.LookupID(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("loook up id of %s : %v", addr, err)
	}

	nv, err := msa.ChainSubmodule.API().StateNetworkVersion(ctx, tsk)
	if err != nil {
		return nil, err
	}

	av, err := actorstypes.VersionForNetwork(nv)
	if err != nil {
		return nil, err
	}

	var dcap abi.StoragePower
	var verified bool
	if av <= 8 {
		vrs, err := view.LoadVerifregActor(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load verified registry state: %v", err)
		}

		verified, dcap, err = vrs.VerifiedClientDataCap(aid)
		if err != nil {
			return nil, fmt.Errorf("looking up verified client: %w", err)
		}
	} else {
		dcs, err := view.LoadDatacapState(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to load datacap actor state: %w", err)
		}

		verified, dcap, err = dcs.VerifiedClientDataCap(aid)
		if err != nil {
			return nil, fmt.Errorf("looking up verified client: %w", err)
		}
	}

	if !verified {
		return nil, nil
	}

	return &dcap, nil
}

func (msa *minerStateAPI) StateChangedActors(ctx context.Context, old cid.Cid, new cid.Cid) (map[string]types.Actor, error) {
	store := msa.ChainReader.Store(ctx)

	oldTree, err := tree.LoadState(ctx, store, old)
	if err != nil {
		return nil, fmt.Errorf("failed to load old state tree: %w", err)
	}

	newTree, err := tree.LoadState(ctx, store, new)
	if err != nil {
		return nil, fmt.Errorf("failed to load new state tree: %w", err)
	}

	return tree.Diff(oldTree, newTree)
}

func (msa *minerStateAPI) StateReadState(ctx context.Context, actor address.Address, tsk types.TipSetKey) (*types.ActorState, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset:%s parent state view: %v", tsk, err)
	}

	act, err := view.LoadActor(ctx, actor)
	if err != nil {
		return nil, err
	}

	blk, err := msa.ChainReader.Blockstore().Get(ctx, act.Head)
	if err != nil {
		return nil, fmt.Errorf("getting actor head: %w", err)
	}

	oif, err := register.DumpActorState(register.GetDefaultActros(), act, blk.RawData())
	if err != nil {
		return nil, fmt.Errorf("dumping actor state (a:%s): %w", actor, err)
	}

	return &types.ActorState{
		Balance: act.Balance,
		Code:    act.Code,
		State:   oif,
	}, nil
}

func (msa *minerStateAPI) StateDecodeParams(ctx context.Context, toAddr address.Address, method abi.MethodNum, params []byte, tsk types.TipSetKey) (interface{}, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset:%s parent state view: %v", tsk, err)
	}

	act, err := view.LoadActor(ctx, toAddr)
	if err != nil {
		return nil, err
	}

	methodMeta, found := utils.MethodsMap[act.Code][method]
	if !found {
		return nil, fmt.Errorf("method %d not found on actor %s", method, act.Code)
	}

	paramType := reflect.New(methodMeta.Params.Elem()).Interface().(cbg.CBORUnmarshaler)

	if err = paramType.UnmarshalCBOR(bytes.NewReader(params)); err != nil {
		return nil, err
	}

	return paramType, nil
}

func (msa *minerStateAPI) StateEncodeParams(ctx context.Context, toActCode cid.Cid, method abi.MethodNum, params json.RawMessage) ([]byte, error) {
	methodMeta, found := utils.MethodsMap[toActCode][method]
	if !found {
		return nil, fmt.Errorf("method %d not found on actor %s", method, toActCode)
	}

	paramType := reflect.New(methodMeta.Params.Elem()).Interface().(cbg.CBORUnmarshaler)

	if err := json.Unmarshal(params, &paramType); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}

	var cbb bytes.Buffer
	if err := paramType.(cbor.Marshaler).MarshalCBOR(&cbb); err != nil {
		return nil, fmt.Errorf("cbor marshal: %w", err)
	}

	return cbb.Bytes(), nil
}

func (msa *minerStateAPI) StateListMessages(ctx context.Context, match *types.MessageMatch, tsk types.TipSetKey, toheight abi.ChainEpoch) ([]cid.Cid, error) {
	ts, err := msa.ChainReader.GetTipSet(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset %s: %w", tsk, err)
	}

	if ts == nil {
		ts = msa.ChainReader.GetHead()
	}

	if match.To == address.Undef && match.From == address.Undef {
		return nil, fmt.Errorf("must specify at least To or From in message filter")
	} else if match.To != address.Undef {
		_, err := msa.StateLookupID(ctx, match.To, tsk)

		// if the recipient doesn't exist at the start point, we're not gonna find any matches
		if errors.Is(err, types.ErrActorNotFound) {
			return nil, nil
		}

		if err != nil {
			return nil, fmt.Errorf("looking up match.To: %w", err)
		}
	} else if match.From != address.Undef {
		_, err := msa.StateLookupID(ctx, match.From, tsk)

		// if the sender doesn't exist at the start point, we're not gonna find any matches
		if errors.Is(err, types.ErrActorNotFound) {
			return nil, nil
		}

		if err != nil {
			return nil, fmt.Errorf("looking up match.From: %w", err)
		}
	}

	// TODO: This should probably match on both ID and robust address, no?
	matchFunc := func(msg *types.Message) bool {
		if match.From != address.Undef && match.From != msg.From {
			return false
		}

		if match.To != address.Undef && match.To != msg.To {
			return false
		}

		return true
	}

	var out []cid.Cid
	for ts.Height() >= toheight {
		msgs, err := msa.MessageStore.MessagesForTipset(ts)
		if err != nil {
			return nil, fmt.Errorf("failed to get messages for tipset (%s): %w", ts.Key(), err)
		}

		for _, msg := range msgs {
			if matchFunc(msg.VMMessage()) {
				out = append(out, msg.Cid())
			}
		}

		if ts.Height() == 0 {
			break
		}

		next, err := msa.ChainReader.GetTipSet(ctx, ts.Parents())
		if err != nil {
			return nil, fmt.Errorf("loading next tipset: %w", err)
		}

		ts = next
	}

	return out, nil
}

// StateMinerAllocated returns a bitfield containing all sector numbers marked as allocated in miner state
func (msa *minerStateAPI) StateMinerAllocated(ctx context.Context, addr address.Address, tsk types.TipSetKey) (*bitfield.BitField, error) {
	_, view, err := msa.Stmgr.ParentStateViewTsk(ctx, tsk)
	if err != nil {
		return nil, fmt.Errorf("loading tipset:%s parent state view: %v", tsk, err)
	}

	act, err := view.LoadActor(ctx, addr)
	if err != nil {
		return nil, err
	}
	mas, err := miner.Load(msa.ChainReader.Store(ctx), act)
	if err != nil {
		return nil, err
	}
	return mas.GetAllocatedSectors()
}
