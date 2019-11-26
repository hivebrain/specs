package storage_mining

import (
	filproofs "github.com/filecoin-project/specs/libraries/filcrypto/filproofs"
	ipld "github.com/filecoin-project/specs/libraries/ipld"
	spc "github.com/filecoin-project/specs/systems/filecoin_blockchain/storage_power_consensus"
	block "github.com/filecoin-project/specs/systems/filecoin_blockchain/struct/block"
	deal "github.com/filecoin-project/specs/systems/filecoin_markets/deal"
	market "github.com/filecoin-project/specs/systems/filecoin_markets/storage_market"
	sector "github.com/filecoin-project/specs/systems/filecoin_mining/sector"
	actor "github.com/filecoin-project/specs/systems/filecoin_vm/actor"
	addr "github.com/filecoin-project/specs/systems/filecoin_vm/actor/address"
	vmr "github.com/filecoin-project/specs/systems/filecoin_vm/runtime"
	exitcode "github.com/filecoin-project/specs/systems/filecoin_vm/runtime/exitcode"
	util "github.com/filecoin-project/specs/util"
)

const (
	Method_StorageMinerActor_SubmitSurprisePoSt = actor.MethodPlaceholder
)

////////////////////////////////////////////////////////////////////////////////
// Boilerplate
////////////////////////////////////////////////////////////////////////////////
type State = StorageMinerActorState
type Any = util.Any
type Bool = util.Bool
type Bytes = util.Bytes
type InvocOutput = vmr.InvocOutput
type Runtime = vmr.Runtime

var TODO = util.TODO

func (a *StorageMinerActorCode_I) State(rt Runtime) (vmr.ActorStateHandle, State) {
	h := rt.AcquireState()
	stateCID := h.Take()
	stateBytes := rt.IpldGet(ipld.CID(stateCID))
	if stateBytes.Which() != vmr.Runtime_IpldGet_FunRet_Case_Bytes {
		rt.Abort("IPLD lookup error")
	}
	state := DeserializeState(stateBytes.As_Bytes())
	return h, state
}
func Release(rt Runtime, h vmr.ActorStateHandle, st State) {
	checkCID := actor.ActorSubstateCID(rt.IpldPut(st.Impl()))
	h.Release(checkCID)
}
func UpdateRelease(rt Runtime, h vmr.ActorStateHandle, st State) {
	newCID := actor.ActorSubstateCID(rt.IpldPut(st.Impl()))
	h.UpdateRelease(newCID)
}
func (st *StorageMinerActorState_I) CID() ipld.CID {
	panic("TODO")
}
func DeserializeState(x Bytes) State {
	panic("TODO")
}

////////////////////////////////////////////////////////////////////////////////

func (a *StorageMinerActorCode_I) _isChallenged(rt Runtime) bool {
	h, st := a.State(rt)
	ret := st._isChallenged()
	Release(rt, h, st)
	return ret
}

func (a *StorageMinerActorCode_I) _canBeElected(rt Runtime) bool {
	h, st := a.State(rt)
	ret := st._canBeElected(rt.CurrEpoch())
	Release(rt, h, st)
	return ret
}

func (a *StorageMinerActorCode_I) _challengeHasExpired(rt Runtime) bool {
	h, st := a.State(rt)
	ret := st._challengeHasExpired(rt.CurrEpoch())
	Release(rt, h, st)
	return ret
}

////////////////////////////////////////////////////////////////////////////////
// Surprise PoSt
////////////////////////////////////////////////////////////////////////////////

// called by StoragePowerActor to notify StorageMiner of PoSt Challenge (triggered by Cron)
func (a *StorageMinerActorCode_I) NotifyOfSurprisePoStChallenge(rt Runtime) InvocOutput {
	rt.ValidateImmediateCallerIs(addr.StoragePowerActorAddr)

	if a._isChallenged(rt) {
		return rt.SuccessReturn() // silent return, dont re-challenge
	}

	a._expirePreCommittedSectors(rt)

	h, st := a.State(rt)
	// update challenge start epoch
	st.ChallengeStatus().Impl().OnNewChallenge(rt.CurrEpoch())
	UpdateRelease(rt, h, st)
	return rt.SuccessReturn()
}

func (a *StorageMinerActorCode_I) _slashDealsForStorageFault(rt Runtime, sectorNumbers []sector.SectorNumber, faultType sector.StorageFaultType) {

	h, st := a.State(rt)

	dealIDs := make([]deal.DealID, 0)

	for _, sectorNo := range sectorNumbers {

		utilizationInfo := st._getUtilizationInfo(rt, sectorNo)
		activeDealIDs := utilizationInfo.DealExpirationAMT().Impl().ActiveDealIDs()
		dealIDs = append(dealIDs, activeDealIDs...)

	}
	processDealSlashParam := make([]actor.MethodParam, 2)

	Release(rt, h, st)
	
	rt.SendCatchingErrors(&msg.InvocInput_I{
		To_:     addr.StorageMarketActorAddr,
		Method_: market.MethodProcessDealSlash,
		Params_: processDealSlashParam,
	})
}

// Called by the cron actor at every tick.
func (a *StorageMinerActorCode_I) OnCronTick(rt Runtime) InvocOutput {
	a.CheckSurprisePoStSubmissionHappened(rt)

	return rt.SuccessReturn()

}

func (a *StorageMinerActorCode_I) _slashPledgeForStorageFault(rt Runtime, sectorNumbers []sector.SectorNumber, faultType sector.StorageFaultType) {
	h, st := a.State(rt)

	affectedPower := block.StoragePower(0)
	for _, sectorNo := range sectorNumbers {

		utilizationInfo := st._getUtilizationInfo(rt, sectorNo)
		affectedPower += utilizationInfo.CurrUtilization()

	}

	Release(rt, h, st)

	panic(affectedPower)
	slashPledgeParams := make([]actor.MethodParam, 3)

	rt.SendCatchingErrors(&msg.InvocInput_I{
		To_:     addr.StoragePowerActorAddr,
		Method_: power.MethodSlashPledgeForStorageFault,
		Params_: slashPledgeParams,
	})
}

// should always happen after _submitPowerReport to have the most up to date utilization info
// reset NewTerminatedFaults
func (a *StorageMinerActorCode_I) _slashCollateralForStorageFaults(
	rt Runtime,
	newDeclaredFaults sector.CompactSectorSet, // diff value
	newDetectedFaults sector.CompactSectorSet, // diff value
	newTerminatedFaults sector.CompactSectorSet, // diff value
) {

	// only terminatedFault will result in collateral deal slashing
	if len(newTerminatedFaults) > 0 {
		a._slashDealsForStorageFault(rt, newTerminatedFaults.SectorsOn(), sector.TerminatedFault)
		a._slashPledgeForStorageFault(rt, newTerminatedFaults.SectorsOn(), sector.TerminatedFault)
	}

	if len(newDetectedFaults) > 0 {
		a._slashPledgeForStorageFault(rt, newDetectedFaults.SectorsOn(), sector.DetectedFault)
	}

	if len(newDeclaredFaults) > 0 {
		a._slashPledgeForStorageFault(rt, newDeclaredFaults.SectorsOn(), sector.DeclaredFault)
	}

	// reset terminated faults
	h, st := a.State(rt)
	st.SectorTable().Impl().TerminatedFaults_ = sector.CompactSectorSet(make([]byte, 0))
	UpdateRelease(rt, h, st)
}

// If the miner fails to respond to a surprise PoSt,
// cron triggers reporting every sector as failing for the current proving period.
func (a *StorageMinerActorCode_I) CheckSurprisePoStSubmissionHappened(rt Runtime) InvocOutput {
	TODO() // TODO: validate caller

	// we can return if miner has not yet been challenged
	if !a._isChallenged(rt) {
		// Miner gets out of a challenge when submit a successful PoSt
		// or when detected by CronActor. Hence, not being in isChallenged means that we are good here
		return rt.SuccessReturn()
	}

	if a._challengeHasExpired(rt) {
		// garbage collection - need to be called by cron once in a while
		a._expirePreCommittedSectors(rt)

		// oh no -- we missed it. rekt
		a._onMissedSurprisePoSt(rt)

<<<<<<< HEAD
=======
	// this will go through even if miners do not have the right amount of pledge collateral
	// when _submitPowerReport is called in DeclareFaults and _onMissedSurprisePoSt for power slashing
	// however in _onSuccessfulPoSt EnsurePledgeCollateralSatsified will be called
	// to ensure that miners have the required pledge collateral
	// otherwise, post submission will fail
	// Note: there is no power update in RecoverFaults and hence no EnsurePledgeCollatera or _submitPowerReport
	rt.SendCatchingErrors(&msg.InvocInput_I{
		To_:     addr.StoragePowerActorAddr,
		Method_: power.MethodProcessPowerReport,
		Params_: processPowerReportParam,
	})

	if len(newExpiredDealIDs) > 0 {
		rt.SendCatchingErrors(&msg.InvocInput_I{
			To_:     addr.StorageMarketActorAddr,
			Method_: market.MethodProcessDealExpiration,
			Params_: processDealExpirationParam,
		})
>>>>>>> wire slashDealsForStorageFault
	}

	return rt.SuccessReturn()
}

// called by CheckSurprisePoSt above for miner who missed their post
func (a *StorageMinerActorCode_I) _onMissedSurprisePoSt(rt Runtime) {
	h, st := a.State(rt)

	failingSectorNumbers := getSectorNums(st.Sectors())
	for _, sectorNo := range failingSectorNumbers {
		st._updateFailSector(rt, sectorNo, true)
	}
	st._updateExpireSectors(rt)
	UpdateRelease(rt, h, st)

	h, st = a.State(rt)

	newDetectedFaults := st.SectorTable().FailingSectors()
	newTerminatedFaults := st.SectorTable().TerminatedFaults()
	lastPoStResponse := st.ChallengeStatus().LastPoStResponseEpoch()

	Release(rt, h, st)

	a._submitPowerReport(rt, lastPoStResponse)

	// Note: NewDetectedFaults is now the sum of all
	// previously active, committed, and recovering sectors minus expired ones
	// and any previously Failing sectors that did not exceed MaxFaultCount
	// Note: previously declared faults is now treated as part of detected faults
	a._slashCollateralForStorageFaults(
		rt,
		sector.CompactSectorSet(make([]byte, 0)), // NewDeclaredFaults
		newDetectedFaults,
		newTerminatedFaults,
	)

	// end of challenge
	// now that new power and faults are tracked move pointer of last challenge response up
	h, st = a.State(rt)
	st.ChallengeStatus().Impl().OnPoStFailure(rt.CurrEpoch())
	st._processStagedCommittedSectors(rt)
	UpdateRelease(rt, h, st)
}

// Decision is to currently account for power based on sector
// with at least one active deals and deals cannot be updated
// an alternative proposal is to account for power based on active deals
// an improvement proposal is to allow storage deal update in a sector

// TODO: decide whether declared faults sectors should be
// penalized in the same way as undeclared sectors and how

// this method is called by both SubmitElectionPoSt and SubmitSurprisePoSt
// - Process ProvingSet.SectorsOn()
//   - State Transitions
//     - Committed -> Active and credit power
//     - Recovering -> Active and credit power
//   - Process Active Sectors (pay miners)
// - Process ProvingSet.SectorsOff()
//     - increment FaultCount
//     - clear Sector and slash pledge collateral if count > MAX_CONSECUTIVE_FAULTS
// - Process Expired Sectors (settle deals and return storage collateral to miners)
//     - State Transition
//       - Failing / Recovering / Active / Committed -> Cleared
//     - Remove SectorNumber from Sectors, ProvingSet
// - Update ChallengeEndEpoch
func (a *StorageMinerActorCode_I) _onSuccessfulPoSt(rt Runtime, onChainInfo sector.OnChainPoStVerifyInfo) InvocOutput {
	h, st := a.State(rt)

	// TODO add info on chain

	// The proof is verified, process ProvingSet.SectorsOn():
	// ProvingSet.SectorsOn() contains SectorCommitted, SectorActive, SectorRecovering
	// ProvingSet itself does not store states, states are all stored in Sectors.State
	for _, sectorNo := range st.Impl().ProvingSet_.SectorsOn() {
		sectorState, found := st.Sectors()[sectorNo]
		if !found {
			// TODO: determine proper error here and error-handling machinery
			rt.Abort("Sector state not found in map")
		}
		switch sectorState.State().StateNumber {
		case SectorCommittedSN, SectorRecoveringSN:
			st._updateActivateSector(rt, sectorNo)
		case SectorActiveSN:
			// do nothing
			// deal payment is made at _onSuccessfulPoSt
		default:
			// TODO: determine proper error here and error-handling machinery
			rt.Abort("Invalid sector state in ProvingSet.SectorsOn()")
		}
	}

	// commit state change so that committed and recovering are now active

	// Process ProvingSet.SectorsOff()
	// ProvingSet.SectorsOff() contains SectorFailing
	// SectorRecovering is Proving and hence will not be in GetZeros()
	// heavy penalty if Failing for more than or equal to MAX_CONSECUTIVE_FAULTS
	// otherwise increment FaultCount in Sectors().State
	for _, sectorNo := range st.Impl().ProvingSet_.SectorsOff() {
		sectorState, found := st.Sectors()[sectorNo]
		if !found {
			continue
		}
		switch sectorState.State().StateNumber {
		case SectorFailingSN:
			st._updateFailSector(rt, sectorNo, true)
		default:
			// TODO: determine proper error here and error-handling machinery
			rt.Abort("Invalid sector state in ProvingSet.SectorsOff")
		}
	}

	// Process Expiration.
	st._updateExpireSectors(rt)

	UpdateRelease(rt, h, st)

	h, st = a.State(rt)
	newTerminatedFaults := st.SectorTable().TerminatedFaults()
	lastPoStResponse := st.ChallengeStatus().LastPoStResponseEpoch()
	Release(rt, h, st)

	a._submitPowerReport(rt, lastPoStResponse)

	a._slashCollateralForStorageFaults(
		rt,
		sector.CompactSectorSet(make([]byte, 0)), // NewDeclaredFaults
		sector.CompactSectorSet(make([]byte, 0)), // NewDetectedFaults
		newTerminatedFaults,
	)

	// Ensure pledge collateral satisfied
	// otherwise, abort _onSuccessfulPoSt
	a._ensurePledgeCollateralSatisfied(rt)

	// Now that all is done update pointer to last response
	h, st = a.State(rt)
	st.ChallengeStatus().Impl().OnPoStSuccess(rt.CurrEpoch())
	st._processStagedCommittedSectors(rt)
	UpdateRelease(rt, h, st)

	return rt.SuccessReturn()

}

// called by verifier to update miner state on successful surprise post
func (a *StorageMinerActorCode_I) SubmitVerifiedSurprisePoSt(rt Runtime, onChainInfo sector.OnChainPoStVerifyInfo) InvocOutput {
	TODO() // TODO: validate caller

	return a._onSuccessfulPoSt(rt, onChainInfo)

}

// Called by StoragePowerConsensus subsystem after verifying the Election proof
// and verifying the PoSt proof in the block header.
// Assume ElectionPoSt has already been successfully verified when the function gets called.
// Likewise assume that the rewards have already been granted to the storage miner actor. This only handles sector management.
func (a *StorageMinerActorCode_I) SubmitVerifiedElectionPoSt(rt Runtime, onChainInfo sector.OnChainPoStVerifyInfo) InvocOutput {

	// TODO: validate caller
	// the caller MUST be the miner who won the block (who won the block should be callable as a a VM runtime call)
	// we also need to enforce that this call happens only once per block, OR make it not callable by special privileged messages
	TODO()

	// we do not need to verify post submission here, as this should have already been done
	// outside of the VM, in StoragePowerConsensus Subsystem. Doing so again would waste
	// significant resources, as proofs are expensive to verify.
	//
	// notneeded := a._verifyPoStSubmission(rt)

	// Update last challenge time as this one, to reset surprise post clock
	h, st := a.State(rt)
	st.ChallengeStatus().Impl().OnNewChallenge(rt.CurrEpoch())

	UpdateRelease(rt, h, st)

	// the following will update last challenge response time
	return a._onSuccessfulPoSt(rt, onChainInfo)

}

////////////////////////////////////////////////////////////////////////////////
// Faults
////////////////////////////////////////////////////////////////////////////////

func (a *StorageMinerActorCode_I) _slashDealsFromFaultReport(rt Runtime, sectorNumbers []sector.SectorNumber, action deal.StorageDealSlashAction) {

	h, st := a.State(rt)

	dealIDs := make([]deal.DealID, 0)

	for _, sectorNo := range sectorNumbers {

		utilizationInfo := st._getUtilizationInfo(rt, sectorNo)
		activeDealIDs := utilizationInfo.DealExpirationAMT().Impl().ActiveDealIDs()
		dealIDs = append(dealIDs, activeDealIDs...)

	}

	Release(rt, h, st)

	// TODO: Send(StorageMarketActor, ProcessDealSlash)
}

// construct FaultReport
// reset NewTerminatedFaults
func (a *StorageMinerActorCode_I) _submitFaultReport(
	rt Runtime,
	newDeclaredFaults sector.CompactSectorSet,
	newDetectedFaults sector.CompactSectorSet,
	newTerminatedFaults sector.CompactSectorSet,
) {
	faultReport := &sector.FaultReport_I{
		NewDeclaredFaults_:   newDeclaredFaults,
		NewDetectedFaults_:   newDetectedFaults,
		NewTerminatedFaults_: newTerminatedFaults,
	}

	rt.Abort("TODO") // TODO: Send(SPA, ProcessFaultReport(faultReport))
	panic(faultReport)

	// only terminatedFault will be slashed
	if len(newTerminatedFaults) > 0 {
		a._slashDealsFromFaultReport(rt, newTerminatedFaults.SectorsOn(), deal.SlashTerminatedFaults)
	}

	h, st := a.State(rt)
	st.SectorTable().Impl().TerminatedFaults_ = sector.CompactSectorSet(make([]byte, 0))
	UpdateRelease(rt, h, st)
}

// construct PowerReport from SectorTable
// need lastPoSt to search for new expired deals in _updateSectorUtilization since the lastPost
// where DealExpirationAMT takes in a range of Epoch and return a list of values that expire in that range
func (a *StorageMinerActorCode_I) _submitPowerReport(rt Runtime, lastPoStResponse block.ChainEpoch) {
	h, st := a.State(rt)
	newExpiredDealIDs := st._updateSectorUtilization(rt, lastPoStResponse)
	activePower := st._getActivePower(rt)
	inactivePower := st._getInactivePower(rt)

	// serialize this in param
	_ = &power.PowerReport_I{
		ActivePower_:   activePower,
		InactivePower_: inactivePower,
	}

	// TODO: serialization helper
	processPowerReportParam := make([]actor.MethodParam, 1)
	processDealExpirationParam := make([]actor.MethodParam, len(newExpiredDealIDs))

	Release(rt, h, st)

	// this will go through even if miners do not have the right amount of pledge collateral
	// when _submitPowerReport is called in DeclareFaults and _onMissedSurprisePoSt for power slashing
	// however in _onSuccessfulPoSt EnsurePledgeCollateralSatsified will be called
	// to ensure that miners have the required pledge collateral
	// Note: there is no power update in RecoverFaults and hence no EnsurePledgeCollatera or _submitPowerReport
	rt.SendCatchingErrors(&msg.InvocInput_I{
		To_:     addr.StoragePowerActorAddr,
		Method_: power.MethodProcessPowerReport,
		Params_: processPowerReportParam,
	})

	if len(newExpiredDealIDs) > 0 {
		rt.SendCatchingErrors(&msg.InvocInput_I{
			To_:     addr.StorageMarketActorAddr,
			Method_: storage_market.MethodProcessDealExpiration,
			Params_: processDealExpirationParam,
		})
	}

}



func (a *StorageMinerActorCode_I) _expirePreCommittedSectors(rt Runtime) {

	h, st := a.State(rt)
	for _, preCommitSector := range st.PreCommittedSectors() {

		elapsedEpoch := rt.CurrEpoch() - preCommitSector.ReceivedEpoch()

		if elapsedEpoch > sector.MAX_PROVE_COMMIT_SECTOR_EPOCH {
			delete(st.PreCommittedSectors(), preCommitSector.Info().SectorNumber())
			// TODO: potentially some slashing if ProveCommitSector comes late
		}
	}
	UpdateRelease(rt, h, st)

}

// RecoverFaults checks if miners have sufficent collateral
// and adds SectorFailing into SectorRecovering
// - State Transition
//   - Failing -> Recovering with the same FaultCount
// - Add SectorNumber to ProvingSet
// Note that power is not updated until it is active
func (a *StorageMinerActorCode_I) RecoverFaults(rt Runtime, recoveringSet sector.CompactSectorSet) InvocOutput {
	TODO() // TODO: validate caller

	// RecoverFaults is only called when miners are not challenged
	if a._isChallenged(rt) {
		rt.Abort("cannot RecoverFaults when sm isChallenged")
	}

	h, st := a.State(rt)

	// for all SectorNumber marked as recovering by recoveringSet
	for _, sectorNo := range recoveringSet.SectorsOn() {
		sectorState, found := st.Sectors()[sectorNo]
		if !found {
			// TODO: determine proper error here and error-handling machinery
			rt.Abort("Sector state not found in map")
		}
		switch sectorState.State().StateNumber {
		case SectorFailingSN:
			// Check if miners have sufficient balances in sma

			// SendMessage(sma.PublishStorageDeals) or sma.ResumeStorageDeals?
			// throw if miner cannot cover StorageDealCollateral

			// copy over the same FaultCount
			st.Sectors()[sectorNo].Impl().State_ = SectorRecovering(sectorState.State().FaultCount)
			st.Impl().ProvingSet_.Add(sectorNo)

			st.SectorTable().Impl().FailingSectors_.Remove(sectorNo)
			st.SectorTable().Impl().RecoveringSectors_.Add(sectorNo)

		default:
			// TODO: determine proper error here and error-handling machinery
			// TODO: consider this a no-op (as opposed to a failure), because this is a user
			// call that may be delayed by the chain beyond some other state transition.
			rt.Abort("Invalid sector state in RecoverFaults")
		}
	}

	UpdateRelease(rt, h, st)

	// EnsureDealCollateral

	return rt.SuccessReturn()
}

// DeclareFaults penalizes miners (slashStorageDealCollateral and remove power)
// TODO: decide how much storage collateral to slash
// - State Transition
//   - Active / Commited / Recovering -> Failing
// - Update State in Sectors()
// - Remove Active / Commited / Recovering from ProvingSet
func (a *StorageMinerActorCode_I) DeclareFaults(rt Runtime, faultSet sector.CompactSectorSet) InvocOutput {
	TODO() // TODO: validate caller

	if a._isChallenged(rt) {
		// TODO: determine proper error here and error-handling machinery
		rt.Abort("cannot DeclareFaults when challenged")
	}

	h, st := a.State(rt)

	// fail all SectorNumber marked as Failing by faultSet
	for _, sectorNo := range faultSet.SectorsOn() {
		st._updateFailSector(rt, sectorNo, false)
	}

	lastPoStResponse := st.ChallengeStatus().LastPoStResponseEpoch()

	UpdateRelease(rt, h, st)

	a._submitPowerReport(rt, lastPoStResponse)

	a._slashCollateralForStorageFaults(
		rt,
		faultSet,                                 // DeclaredFaults
		sector.CompactSectorSet(make([]byte, 0)), // DetectedFaults
		sector.CompactSectorSet(make([]byte, 0)), // TerminatedFault
	)

	return rt.SuccessReturn()
}

////////////////////////////////////////////////////////////////////////////////
// Sector Commitment
////////////////////////////////////////////////////////////////////////////////

func (a *StorageMinerActorCode_I) _verifySeal(rt Runtime, onChainInfo sector.OnChainSealVerifyInfo) bool {
	h, st := a.State(rt)
	info := st.Info()
	sectorSize := info.SectorSize()
	dealIDs := onChainInfo.DealIDs()
	params := make([]actor.MethodParam, 1+len(dealIDs))

	Release(rt, h, st) // if no modifications made; or

	// TODO: serialize method param as {sectorSize,  DealIDs...}.
	receipt := rt.SendCatchingErrors(&vmr.InvocInput_I{
		To_:     addr.StorageMarketActorAddr,
		Method_: market.MethodGetUnsealedCIDForDealIDs,
		Params_: params,
	})

	if receipt.ExitCode() == exitcode.InvalidSectorPacking {
		return false
	}

	ret := receipt.ReturnValue()
	pieceInfos := sector.PieceInfosFromBytes(ret)

	// Unless we enforce a minimum padding amount, this totalPieceSize calculation can be removed.
	// Leaving for now until that decision is entirely finalized.
	var totalPieceSize util.UInt
	for _, pieceInfo := range pieceInfos {
		pieceSize := (*pieceInfo).Size()
		totalPieceSize += pieceSize
	}

	unsealedCID, _ := filproofs.ComputeUnsealedSectorCIDFromPieceInfos(sectorSize, pieceInfos)

	sealCfg := sector.SealCfg_I{
		SectorSize_:  sectorSize,
		WindowCount_: info.WindowCount(),
		Partitions_:  info.SealPartitions(),
	}
	svInfo := sector.SealVerifyInfo_I{
		SectorID_: &sector.SectorID_I{
			MinerID_: info.Worker(), // TODO: This is actually miner address. MinerID needs to be derived.
			Number_:  onChainInfo.SectorNumber(),
		},
		OnChain_: onChainInfo,

		// TODO: Make SealCfg sector.SealCfg from miner configuration (where is that?)
		SealCfg_: &sealCfg,

		Randomness_:            sector.SealRandomness(rt.Randomness(onChainInfo.SealEpoch(), 0)),
		InteractiveRandomness_: sector.InteractiveSealRandomness(rt.Randomness(onChainInfo.InteractiveEpoch(), 0)),
		UnsealedCID_:           unsealedCID,
	}

	sdr := filproofs.WinSDRParams(&filproofs.SDRCfg_I{SealCfg_: &sealCfg})
	return sdr.VerifySeal(&svInfo)
}

// Deals must be posted on chain via sma.PublishStorageDeals before PreCommitSector
// TODO(optimization): PreCommitSector could contain a list of deals that are not published yet.
func (a *StorageMinerActorCode_I) PreCommitSector(rt Runtime, info sector.SectorPreCommitInfo) InvocOutput {
	TODO() // TODO: validate caller

	// can be called regardless of Challenged status

	// TODO: might take collateral in case no ProveCommit follows within sometime
	// TODO: collateral also penalizes repeated precommit to get randomness that one likes
	// TODO: might be a good place for Treasury

	h, st := a.State(rt)

	_, found := st.PreCommittedSectors()[info.SectorNumber()]

	if found {
		// TODO: burn some funds?
		rt.Abort("Sector already pre committed.")
	}

	st._assertSectorDidNotExist(rt, info.SectorNumber())

	Release(rt, h, st)

	// verify every DealID has been published and will not expire
	// before the MAX_PROVE_COMMIT_SECTOR_EPOCH + CurrEpoch
	// abort otherwise
	// Send(SMA.VerifyPublishedDealIDs(info.DealIDs()))

	h, st = a.State(rt)

	precommittedSector := &PreCommittedSector_I{
		Info_:          info,
		ReceivedEpoch_: rt.CurrEpoch(),
	}
	st.PreCommittedSectors()[info.SectorNumber()] = precommittedSector

	UpdateRelease(rt, h, st)
	return rt.SuccessReturn()
}

func (a *StorageMinerActorCode_I) ProveCommitSector(rt Runtime, info sector.SectorProveCommitInfo) InvocOutput {
	TODO() // TODO: validate caller

	h, st := a.State(rt)

	preCommitSector, precommitFound := st.PreCommittedSectors()[info.SectorNumber()]

	if !precommitFound {
		rt.Abort("Sector not pre committed.")
	}

	st._assertSectorDidNotExist(rt, info.SectorNumber())

	// check if ProveCommitSector comes too late after PreCommitSector
	elapsedEpoch := rt.CurrEpoch() - preCommitSector.ReceivedEpoch()

	// if more than MAX_PROVE_COMMIT_SECTOR_EPOCH has elapsed
	if elapsedEpoch > sector.MAX_PROVE_COMMIT_SECTOR_EPOCH {
		// TODO: potentially some slashing if ProveCommitSector comes late

		// TODO: remove dealIDs from PublishedDeals

		// expired
		delete(st.PreCommittedSectors(), preCommitSector.Info().SectorNumber())
		UpdateRelease(rt, h, st)
		return rt.ErrorReturn(exitcode.UserDefinedError(0)) // TODO: user dfined error code?
	}

	onChainInfo := &sector.OnChainSealVerifyInfo_I{
		SealedCID_:        preCommitSector.Info().SealedCID(),
		SealEpoch_:        preCommitSector.Info().SealEpoch(),
		InteractiveEpoch_: info.InteractiveEpoch(),
		Proof_:            info.Proof(),
		DealIDs_:          preCommitSector.Info().DealIDs(),
		SectorNumber_:     preCommitSector.Info().SectorNumber(),
	}

	isSealVerified := st._verifySeal(rt, onChainInfo)
	if !isSealVerified {
		// TODO: determine proper error here and error-handling machinery
		rt.Abort("Seal verification failed")
	}

	_, utilizationFound := st.SectorUtilization()[onChainInfo.SectorNumber()]
	if utilizationFound {
		rt.Abort("sm.ProveCommitSector: sector number found in SectorUtilization")
	}

	// ActivateStorageDeals
	// Ok if deal has started
	// Send(SMA.ActivateDeals(onChainInfo.DealIDs())
	// abort if activation failed
	var deals []deal.OnChainDeal
	initialUtilization := st._initializeUtilizationInfo(rt, deals)
	lastDealExpiration := initialUtilization.DealExpirationAMT().Impl().LastDealExpiration()

	// add sector expiration to SectorExpirationQueue
	st.SectorExpirationQueue().Add(&SectorExpirationQueueItem_I{
		SectorNumber_: onChainInfo.SectorNumber(),
		Expiration_:   lastDealExpiration,
	})

	// no need to store the proof and randomseed in the state tree
	// verify and drop, only SealCommitment{CommR, DealIDs} on chain
	sealCommitment := &sector.SealCommitment_I{
		SealedCID_:  onChainInfo.SealedCID(),
		DealIDs_:    onChainInfo.DealIDs(),
		Expiration_: lastDealExpiration, // TODO decide if we need this too
	}

	// add SectorNumber and SealCommitment to Sectors
	// set Sectors.State to SectorCommitted
	// Note that SectorNumber will only become Active at the next successful PoSt
	sealOnChainInfo := &SectorOnChainInfo_I{
		SealCommitment_: sealCommitment,
		State_:          SectorCommitted(),
	}

	if a._isChallenged(rt) {
		// move PreCommittedSector to StagedCommittedSectors if in Challenged status
		stagedSectorInfo := &StagedCommittedSectorInfo_I{
			Sector_:      sealOnChainInfo,
			Utilization_: initialUtilization,
		}

		st.StagedCommittedSectors()[onChainInfo.SectorNumber()] = stagedSectorInfo
	} else {
		// move PreCommittedSector to CommittedSectors if not in Challenged status
		st.Sectors()[onChainInfo.SectorNumber()] = sealOnChainInfo
		st.Impl().ProvingSet_.Add(onChainInfo.SectorNumber())
		st.SectorTable().Impl().CommittedSectors_.Add(onChainInfo.SectorNumber())
		st.SectorUtilization()[onChainInfo.SectorNumber()] = initialUtilization
	}

	// now remove SectorNumber from PreCommittedSectors (processed)
	delete(st.PreCommittedSectors(), preCommitSector.Info().SectorNumber())
	UpdateRelease(rt, h, st)

	return rt.SuccessReturn()
}

func (a *StorageMinerActorCode_I) _ensurePledgeCollateralSatisfied(rt Runtime) {
	emptyParams := make([]actor.MethodParam, 0)
	ret := rt.SendCatchingErrors(&msg.InvocInput_I{
		To_:     addr.StoragePowerActorAddr,
		Method_: power.EnsurePledgeCollateralSatisfied,
		Params_: emptyParams,
	})

	if ret.ExitCode() == exitcode.InsufficientPledgeCollateral {
		rt.Abort("sma._onSuccessfulPoSt: insufficient pledge collateral.")
	}
}

func getSectorNums(m map[sector.SectorNumber]SectorOnChainInfo) []sector.SectorNumber {
	var l []sector.SectorNumber
	for i, _ := range m {
		l = append(l, i)
	}
	return l
}
