package interpreter

import (
	ipld "github.com/filecoin-project/specs/libraries/ipld"
	actor "github.com/filecoin-project/specs/systems/filecoin_vm/actor"
	addr "github.com/filecoin-project/specs/systems/filecoin_vm/actor/address"
	ai "github.com/filecoin-project/specs/systems/filecoin_vm/actor_interfaces"
	actor_util "github.com/filecoin-project/specs/systems/filecoin_vm/actor_util"
	msg "github.com/filecoin-project/specs/systems/filecoin_vm/message"
	vmr "github.com/filecoin-project/specs/systems/filecoin_vm/runtime"
	exitcode "github.com/filecoin-project/specs/systems/filecoin_vm/runtime/exitcode"
	gascost "github.com/filecoin-project/specs/systems/filecoin_vm/runtime/gascost"
	vmri "github.com/filecoin-project/specs/systems/filecoin_vm/runtime/impl"
	st "github.com/filecoin-project/specs/systems/filecoin_vm/state_tree"
	sysactors "github.com/filecoin-project/specs/systems/filecoin_vm/sysactors"
	util "github.com/filecoin-project/specs/util"
)

type Bytes = util.Bytes

var Assert = util.Assert
var TODO = util.TODO
var IMPL_FINISH = util.IMPL_FINISH

type SenderResolveSpec int

const (
	SenderResolveSpec_OK SenderResolveSpec = 1 + iota
	SenderResolveSpec_Invalid
)

// Placeholder for an IPLD tree store which is provided to the interpreter.
var store vmri.IPLDStore

// Applies all the message in a tipset, along with implicit block- and tipset-specific state
// transitions.
func (vmi *VMInterpreter_I) ApplyTipSetMessages(inTree st.StateTree, msgs TipSetMessages) (outTree st.StateTree, receipts []vmr.MessageReceipt) {
	outTree = inTree
	seenMsgs := make(map[ipld.CID]struct{}) // CIDs of messages already seen once.
	var receipt vmr.MessageReceipt

	for _, blk := range msgs.Blocks() {
		minerAddr := blk.Miner()
		util.Assert(minerAddr.IsIDType()) // Block syntactic validation requires this.

		// Process block miner's Election PoSt.
		epostMessage := _makeElectionPoStMessage(outTree, minerAddr)
		outTree = _applyMessageBuiltinAssert(outTree, epostMessage, minerAddr)

		minerPenaltyTotal := actor.TokenAmount(0)
		var minerPenaltyCurr actor.TokenAmount

		// Process BLS messages from the block.
		for _, m := range blk.BLSMessages() {
			_, found := seenMsgs[_msgCID(m)]
			if found {
				continue
			}
			onChainMessageLen := len(msg.Serialize_UnsignedMessage(m))
			outTree, receipt, minerPenaltyCurr = vmi.ApplyMessage(outTree, m, onChainMessageLen, minerAddr)
			minerPenaltyTotal += minerPenaltyCurr
			receipts = append(receipts, receipt)
			seenMsgs[_msgCID(m)] = struct{}{}
		}

		// Process SECP messages from the block.
		for _, sm := range blk.SECPMessages() {
			m := sm.Message()
			_, found := seenMsgs[_msgCID(m)]
			if found {
				continue
			}
			onChainMessageLen := len(msg.Serialize_SignedMessage(sm))
			outTree, receipt, minerPenaltyCurr = vmi.ApplyMessage(outTree, m, onChainMessageLen, minerAddr)
			minerPenaltyTotal += minerPenaltyCurr
			receipts = append(receipts, receipt)
			seenMsgs[_msgCID(m)] = struct{}{}
		}

		// Pay block reward.
		rewardMessage := _makeBlockRewardMessage(outTree, minerAddr, minerPenaltyTotal)
		outTree = _applyMessageBuiltinAssert(outTree, rewardMessage, minerAddr)
	}

	// Invoke cron tick.
	// Since this is outside any block, the top level block winner is declared as the system actor.
	cronMessage := _makeCronTickMessage(outTree)
	outTree = _applyMessageBuiltinAssert(outTree, cronMessage, addr.SystemActorAddr)

	return
}

func (vmi *VMInterpreter_I) ApplyMessage(
	inTree st.StateTree, message msg.UnsignedMessage, onChainMessageSize int, minerAddr addr.Address) (
	retTree st.StateTree, retReceipt vmr.MessageReceipt, retMinerPenalty actor.TokenAmount) {

	senderAddr := _resolveSender(inTree, message.From())
	minerOwner := _lookupMinerOwner(inTree, minerAddr)
	Assert(minerOwner.IsIDType())

	vmiGasRemaining := message.GasLimit()
	vmiGasUsed := msg.GasAmount_Zero()

	_applyReturn := func(
		tree st.StateTree, invocOutput vmr.InvocOutput, exitCode exitcode.ExitCode,
		senderResolveSpec SenderResolveSpec) {

		vmiGasRemainingFIL := _gasToFIL(vmiGasRemaining, message.GasPrice())
		vmiGasUsedFIL := _gasToFIL(vmiGasUsed, message.GasPrice())

		switch senderResolveSpec {
		case SenderResolveSpec_OK:
			// In this case, the sender is valid and has already transferred funds to the burnt funds actor
			// sufficient for the gas limit. Thus, we may refund the unused gas funds to the sender here.
			Assert(!message.GasLimit().LessThan(vmiGasUsed))
			Assert(message.GasLimit().Equals(vmiGasUsed.Add(vmiGasRemaining)))
			tree = _withTransferFundsAssert(tree, addr.BurntFundsActorAddr, senderAddr, vmiGasRemainingFIL)
			tree = _withTransferFundsAssert(tree, addr.BurntFundsActorAddr, minerOwner, vmiGasUsedFIL)
			retMinerPenalty = actor.TokenAmount(0)

		case SenderResolveSpec_Invalid:
			retMinerPenalty = vmiGasUsedFIL

		default:
			Assert(false)
		}

		retTree = tree
		retReceipt = vmr.MessageReceipt_Make(invocOutput, exitCode, vmiGasUsed)
	}

	_applyError := func(tree st.StateTree, errExitCode exitcode.SystemErrorCode, senderResolveSpec SenderResolveSpec) {
		_applyReturn(tree, vmr.InvocOutput_Make(nil), exitcode.SystemError(errExitCode), senderResolveSpec)
	}

	// Deduct an amount of gas corresponding to cost about to be incurred, but not necessarily
	// incurred yet.
	_vmiAllocGas := func(amount msg.GasAmount) (vmiAllocGasOK bool) {
		vmiGasRemaining, vmiAllocGasOK = vmiGasRemaining.SubtractIfNonnegative(amount)
		vmiGasUsed = message.GasLimit().Subtract(vmiGasRemaining)
		Assert(!vmiGasRemaining.LessThan(msg.GasAmount_Zero()))
		Assert(!vmiGasUsed.LessThan(msg.GasAmount_Zero()))
		return
	}

	// Deduct an amount of gas corresponding to costs already incurred, and for which the
	// gas cost must be paid even if it would cause the gas used to exceed the limit.
	_vmiBurnGas := func(amount msg.GasAmount) (vmiBurnGasOK bool) {
		vmiGasUsedPre := vmiGasUsed
		vmiBurnGasOK = _vmiAllocGas(amount)
		if !vmiBurnGasOK {
			vmiGasRemaining = msg.GasAmount_Zero()
			vmiGasUsed = vmiGasUsedPre.Add(amount)
		}
		return
	}

	ok := _vmiBurnGas(gascost.OnChainMessage(onChainMessageSize))
	if !ok {
		// Invalid message; insufficient gas limit to pay for the on-chain message size.
		_applyError(inTree, exitcode.OutOfGas, SenderResolveSpec_Invalid)
		return
	}

	fromActor, ok := inTree.GetActor(senderAddr)
	if !ok {
		// Execution error; sender does not exist at time of message execution.
		_applyError(inTree, exitcode.ActorNotFound, SenderResolveSpec_Invalid)
		return
	}

	// make sure this is the right message order for fromActor
	if message.CallSeqNum() != fromActor.CallSeqNum() {
		_applyError(inTree, exitcode.InvalidCallSeqNum, SenderResolveSpec_Invalid)
		return
	}

	// Check sender balance.
	gasLimitCost := _gasToFIL(message.GasLimit(), message.GasPrice())
	totalCost := message.Value() + actor.TokenAmount(gasLimitCost)
	if fromActor.Balance() < totalCost {
		// Execution error; sender does not have sufficient funds to pay for the gas limit.
		_applyError(inTree, exitcode.InsufficientFunds_System, SenderResolveSpec_Invalid)
		return
	}

	// At this point, construct compTreePreSend as a state snapshot which includes
	// the sender paying gas, and the sender's CallSeqNum being incremented;
	// at least that much state change will be persisted even if the
	// method invocation subsequently fails.
	compTreePreSend := _withTransferFundsAssert(inTree, senderAddr, addr.BurntFundsActorAddr, gasLimitCost)
	compTreePreSend = compTreePreSend.Impl().WithIncrementedCallSeqNum_Assert(senderAddr)

	// Reload sender actor state after the transfer and CallSeqNum increment.
	sender, ok := compTreePreSend.GetActor(senderAddr)
	Assert(ok)
	invoc := _makeInvocInput(message)
	sendRet, compTreePostSend := _applyMessageInternal(compTreePreSend, sender, senderAddr, invoc, vmiGasRemaining, minerAddr)

	ok = _vmiBurnGas(sendRet.GasUsed())
	if !ok {
		panic("Interpreter error: runtime execution used more gas than provided")
	}

	ok = _vmiAllocGas(gascost.OnChainReturnValue(sendRet.ReturnValue()))
	if !ok {
		// Insufficient gas remaining to cover the on-chain return value; proceed as in the case
		// of method execution failure.
		_applyError(compTreePreSend, exitcode.OutOfGas, SenderResolveSpec_OK)
		return
	}

	compTreeRet := compTreePreSend
	if sendRet.ExitCode().AllowsStateUpdate() {
		compTreeRet = compTreePostSend
	}

	_applyReturn(
		compTreeRet, vmr.InvocOutput_Make(sendRet.ReturnValue()), sendRet.ExitCode(), SenderResolveSpec_OK)
	return
}

// Resolves an address through the InitActor's map.
// Returns the resolved address (which will be an ID address) if found, else the original address.
func _resolveSender(tree st.StateTree, address addr.Address) addr.Address {
	initState, ok := tree.GetActor(addr.InitActorAddr)
	util.Assert(ok)
	serialized, ok := store.Get(ipld.CID(initState.State()))
	initSubState := sysactors.Deserialize_InitActorState_Assert(serialized)
	return initSubState.ResolveAddress(address)
}

// Get the owner account address associated to a given miner actor.
func _lookupMinerOwner(tree st.StateTree, minerAddr addr.Address) addr.Address {
	initState, ok := tree.GetActor(minerAddr)
	util.Assert(ok)
	serialized, ok := store.Get(ipld.CID(initState.State()))
	// This tiny coupling between the VM and the storage miner actor is unfortunate.
	// It could be avoided by:
	// - including the owner address in block headers (and requiring it match the block's miner as part of semantic validation)
	// - paying rewards to the miner actor instead of owner (with some miner->owner withdrawal mechanism)
	minerSubState := storage_mining.Deserialize_StorageMinerActorState_Assert(serialized)
	return minerSubState.Info().Owner()
}

func _applyMessageBuiltinAssert(tree st.StateTree, message msg.UnsignedMessage, minerAddr addr.Address) st.StateTree {
	senderAddr := message.From()
	Assert(senderAddr.Equals(addr.SystemActorAddr))
	Assert(senderAddr.IsIDType())
	// Note: this message CallSeqNum is never checked (b/c it's created in this file), but probably should be.
	// Since it changes state, we should be sure about the state transition.
	// Alternatively we could special-case the system actor and declare that its CallSeqNumber
	// never changes (saving us the state-change overhead).
	tree = tree.Impl().WithIncrementedCallSeqNum_Assert(senderAddr)

	sender, ok := tree.GetActor(senderAddr)
	Assert(ok)
	invoc := _makeInvocInput(message)
	retReceipt, retTree := _applyMessageInternal(tree, sender, senderAddr, invoc, message.GasLimit(), minerAddr)
	if retReceipt.ExitCode() != exitcode.OK() {
		panic("internal message application failed")
	}

	return retTree
}

func _applyMessageInternal(tree st.StateTree, sender actor.ActorState, senderAddr addr.Address, invoc vmr.InvocInput,
	gasRemainingInit msg.GasAmount, topLevelBlockWinner addr.Address) (vmr.MessageReceipt, st.StateTree) {

	rt := vmri.VMContext_Make(
		store,
		senderAddr,
		topLevelBlockWinner,
		sender.CallSeqNum(),
		actor.CallSeqNum(0),
		tree,
		senderAddr,
		actor.TokenAmount(0),
		gasRemainingInit,
	)

	return rt.SendToplevelFromInterpreter(invoc)
}

func _withTransferFundsAssert(tree st.StateTree, from addr.Address, to addr.Address, amount actor.TokenAmount) st.StateTree {
	// TODO: assert amount nonnegative
	retTree, err := tree.Impl().WithFundsTransfer(from, to, amount)
	if err != nil {
		panic("Interpreter error: insufficient funds (or transfer error) despite checks")
	} else {
		return retTree
	}
}

func _gasToFIL(gas msg.GasAmount, price actor.TokenAmount) actor.TokenAmount {
	IMPL_FINISH()
	panic("") // BigInt arithmetic
	// return actor.TokenAmount(util.UVarint(gas) * util.UVarint(price))
}

func _makeInvocInput(message msg.UnsignedMessage) vmr.InvocInput {
	return &vmr.InvocInput_I{
		To_:     message.To(), // Receiver address is resolved during execution.
		Method_: message.Method(),
		Params_: message.Params(),
		Value_:  message.Value(),
	}
}

// Builds a message for paying block reward to a miner's owner.
func _makeBlockRewardMessage(state st.StateTree, minerAddr addr.Address, penalty actor.TokenAmount) msg.UnsignedMessage {
	params := make([]util.Serialization, 2)
	params[0] = addr.Serialize_Address(minerAddr)
	params[1] = actor.Serialize_TokenAmount(penalty)

	TODO() // serialize other inputs to BlockRewardMessage or get this from query in RewardActor

	sysActor, ok := state.GetActor(addr.SystemActorAddr)
	Assert(ok)
	return &msg.UnsignedMessage_I{
		From_:       addr.SystemActorAddr,
		To_:         addr.RewardActorAddr,
		Method_:     ai.Method_RewardActor_AwardBlockReward,
		Params_:     params,
		CallSeqNum_: sysActor.CallSeqNum(),
		Value_:      0,
		GasPrice_:   0,
		GasLimit_:   msg.GasAmount_SentinelUnlimited(),
	}
}

// Builds a message for submitting ElectionPost on behalf of a miner actor.
func _makeElectionPoStMessage(state st.StateTree, minerActorAddr addr.Address) msg.UnsignedMessage {
	// TODO: determine parameters necessary for this message.
	params := make([]util.Serialization, 0)

	sysActor, ok := state.GetActor(addr.SystemActorAddr)
	Assert(ok)
	return &msg.UnsignedMessage_I{
		From_:       addr.SystemActorAddr,
		To_:         minerActorAddr,
		Method_:     ai.Method_StorageMinerActor_OnVerifiedElectionPoSt,
		Params_:     params,
		CallSeqNum_: sysActor.CallSeqNum(),
		Value_:      0,
		GasPrice_:   0,
		GasLimit_:   msg.GasAmount_SentinelUnlimited(),
	}
}

// Builds a message for invoking the cron actor tick.
func _makeCronTickMessage(state st.StateTree) msg.UnsignedMessage {
	sysActor, ok := state.GetActor(addr.SystemActorAddr)
	Assert(ok)
	return &msg.UnsignedMessage_I{
		From_:       addr.SystemActorAddr,
		To_:         addr.CronActorAddr,
		Method_:     sysactors.Method_CronActor_EpochTick,
		Params_:     nil,
		CallSeqNum_: sysActor.CallSeqNum(),
		Value_:      0,
		GasPrice_:   0,
		GasLimit_:   msg.GasAmount_SentinelUnlimited(),
	}
}

func _msgCID(msg msg.UnsignedMessage) ipld.CID {
	panic("TODO")
}
