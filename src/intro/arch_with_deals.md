---
title: "Architecture Diagram"
---

## Overview Diagram

<img src="overview.svg" />

## Storage Flow

With deals

{{% mermaid %}}
sequenceDiagram

    participant RetrievalClient
    participant RetrievalProvider

    participant StorageClient
    participant StorageProvider

    participant PaymentChannelActor
    participant PaymentsSubsystem

    participant BlockchainSubsystem
    participant BlockSyncer
    participant BlockProducer

    participant StoragePowerConsensusSubsystem
    participant StoragePowerActor

    participant StorageMiningSubsystem
    participant StorageMinerActor
    participant SectorIndexerSubsystem
    participant StorageProvingSubsystem

    participant FilecoinProofsSubsystem
    participant ClockSubsystem
    participant libp2p

    Note over RetrievalClient,RetrievalProvider: RetrievalMarketSubsystem
    Note over StorageClient,StorageProvider: StorageMarketSubsystem
    Note over BlockchainSubsystem,StoragePowerActor: BlockchainGroup
    Note over StorageMiningSubsystem,StorageProvingSubsystem: MiningGroup

    opt RetrievalDealMake
        RetrievalClient ->>+ RetrievalProvider: DealProposal
        RetrievalProvider -->>- RetrievalClient: {Accepted, Rejected}
    end

    opt RetrievalQuery
        RetrievalClient ->>+ RetrievalProvider: Query(CID)
        RetrievalProvider -->>- RetrievalClient: MinPrice, Unavail
    end

    opt RegisterStorageMiner
        StorageMiningSubsystem ->> StorageMiningSubsystem: CreateMiner(ownerPubKey PubKey, workerPubKey PubKey, pledgeAmt TokenAmount)
        StorageMiningSubsystem ->>+ StoragePowerActor: RegisterMiner(OwnerAddr, WorkerPubKey)
        StoragePowerActor -->>- StorageMiningSubsystem: StorageMinerActor
    end

    opt StorageDealMake
        Note left of StorageClient: Piece, PieceCID
        StorageClient ->> StorageProvider: ProposeStorageDeal(StorageDealProposal)
        StorageClient ->>+ StorageProvider: QueryStorageDealStatus(StorageDealQuery)
        StorageProvider -->>- StorageClient: StorageDealResponse{StorageDealAccepted, Deal} 
         
        Note left of StorageClient: Piece, PieceCID, Deal
        Note right of StorageProvider: Piece, PieceCID, Deal
        StorageClient ->>+ StorageProvider: QueryStorageDealStatus(StorageDealQuery)
        StorageProvider -->>- StorageClient: StorageDealResponse{StorageDealStarted, Deal}
        alt StorageClient notifies network
            StorageClient ->> StorageMarketActor: NotifyOfDeal(Deal, DealStatusPending)
        else 
            StorageProvider ->>  StorageMarketActor: NotifyOfDeal(Deal, DealStatusPending)
        end
        StorageMarketActor -->>- StorageMarketActor: AddDeal(Deal, DealStatusPending)
    end

    opt AddingDealToSector
        StorageProvider ->>+ StorageMiningSubsystem: HandleStorageDeal(Deal, PieceRef)
        StorageMiningSubsystem ->>+ SectorIndexerSubsystem: AddPieceToSector(Deal, SectorID)
        SectorIndexerSubsystem ->> SectorIndexerSubsystem: IndexSectorByDealExpiration(SectorID, Deal)
        SectorIndexerSubsystem -->>- StorageMiningSubsystem: (SectorID, Deal)
        StorageMiningSubsystem ->>- StorageProvider: NotifyStorageDealStaged(Deal,PieceRef,SectorID)
    end

    opt ClientQuery
        StorageClient ->>+ StorageProvider: QueryStorageDealStatus(StorageDealQuery)
        StorageProvider -->>- StorageClient: StorageDealResponse{StorageDealStaged,Deal}
    end

    opt SealingSector
        StorageMiningSubsystem ->>+ StoragePowerConsensusSubsystem: GetSealSeed(Chain, Epoch)
        StoragePowerConsensusSubsystem -->>- StorageMiningSubsystem: Seed
        StorageMiningSubsystem ->>+ StorageProvingSubsystem: SealSector(Seed, SectorID, ReplicaCfg)
        StorageProvingSubsystem ->>+ SectorSealer: Seal(Seed, SectorID, ReplicaCfg)
        SectorSealer -->>- StorageProvingSubsystem: SealOutputs
        StorageProvingSubsystem ->>- StorageMiningSubsystem: SealOutputs
        opt CommitSector
            StorageMiningSubsystem ->> StorageMinerActor: CommitSector(Seed, SectorID, SealCommitment, SealProof, [&Deal], [Deal])
            StorageMinerActor ->>+ FilecoinProofsSubsystem: VerifySeal(SectorID, OnSectorInfo)
            FilecoinProofsSubsystem -->>- StorageMinerActor: {1,0}
            alt 1 - success
                StorageMinerActor ->> StorageMinerActor: AddDeal(SectorID, [&Deal], DealStatusOnChain)
                StorageMinerActor ->> StorageMinerActor: AddDeal(SectorID, [Deal], DealStatusPending)
                StorageMinerActor ->> StoragePowerActor: IncrementPower(StorageMiner.WorkerPubKey)
            else 0 - failure
                StorageMinerActor -->> StorageMiningSubsystem: CommitSectorError
            end
        end
    end

    loop PoStSubmission
        Note Right of PoStSubmission: in every proving period
        Note Right of PoStSubmission: DoneSet
        StorageMiningSubsystem ->>+ StoragePowerConsensusSubsystem: GetPoStChallenge(Chain, Epoch)
        StoragePowerConsensusSubsystem -->>- StorageMiningSubsystem: challenge
        StorageMiningSubsystem ->>+ StorageProvingSubsystem: GeneratePoSt(challenge, [SectorID])
        StorageProvingSubsystem ->>+ StorageProvingSubsystem: GeneratePoSt(challenge, [SectorID])
        StorageMiningSubsystem ->>+ StorageProvingSubsystem: GeneratePoSt(challenge, [SectorID])
        StorageProvingSubsystem -->>- StorageMiningSubsystem: PoStProof
        opt SubmitPoSt
            StorageMiningSubsystem ->> StorageMinerActor: SubmitPoSt(PoStProof, DoneSet)
            StorageMinerActor ->>+ FilecoinProofsSubsystem: VerifyPoSt(PoStProof)
            FilecoinProofsSubsystem -->>- StorageMinerActor: {1,0}
            alt 1 - success
                StorageMinerActor ->> StorageMinerActor:  UpdateDoneSet()
                StorageMinerActor ->> MarketMinerActor:  EnsureDealsAreAccountedFor()
                StorageMarketActor ->> StorageMarketActor: ProcessDealPayment(Deal.Frequency, Deal.Expiration, Deal.Amount)
            else 0 - failure
                StorageMinerActor -->> StorageMiningSubsystem: PoStError
            end
        end
    end

    opt ClientQuery
        StorageClient ->>+ StorageProvider: QueryStorageDealStatus(StorageDealQuery)
        StorageProvider -->>- StorageClient: StorageDealResponse{SealingParams,DealComplete,...}
    end

    loop BlockReception
        BlockSyncer ->>+ libp2p: Subscribe(OnNewBlock)
        libp2p -->>- BlockSyncer: Event(OnNewBlock, block)
        BlockSyncer ->> BlockSyncer: ValidateSyntax(block)
        BlockSyncer ->>+ BlockchainSubsystem: HandleBlock(block)
        BlockchainSubsystem ->> BlockchainSubsystem: ValidateBlock(block)
        BlockchainSubsystem ->> StoragePowerConsensusSubsystem: ValidateBlock(block)
        BlockchainSubsystem ->> FilecoinProofsSubsystem: ValidateBlock(block)
        BlockchainSubsystem ->>- BlockchainSubsystem: StateTree ← TryGenerateStateTree(block)

        alt Round Cutoff
            WallClock -->> BlockchainSubsystem: AssembleTipsets()
            BlockchainSubsystem ->> BlockchainSubsystem: [Tipset] ← AssembleTipsets()
            BlockchainSubsystem ->> BlockchainSubsystem: Tipset ← ChooseTipset([Tipset])
            BlockchainSubsystem ->> Blockchain: ApplyStateTree(StateTree)
        end
    end

    loop BlockProduction
        alt New Tipset
            BlockchainSubsystem ->> StorageMiningSubsystem: OnNewTipset(Chain, Epoch)
        else Null block last round
            WallClock ->> StorageMiningSubsystem: OnNewRound()
            Note Right of WallClock: epoch is incremented by 1
        end
        StorageMiningSubsystem ->>+ StoragePowerConsensusSubsystem: GetElectionArtifacts(Chain, Epoch)
        StoragePowerConsensusSubsystem ->> StoragePowerConsensusSubsystem: TK ← TicketAtEpoch(Chain, Epoch-k)
        StoragePowerConsensusSubsystem ->> StoragePowerConsensusSubsystem: T1 ← TicketAtEpoch(Chain, Epoch-1)
        StoragePowerConsensusSubsystem -->>- StorageMiningSubsystem: TK, T1
       
        loop forEach StorageMiningSubsystem.StorageMiner
            StorageMiningSubsystem ->> StorageMiningSubsystem: EP ← DrawElectionProof(TK.randomness(), StorageMiner.WorkerKey)
            alt New Tipset
                StorageMiningSubsystem ->> StorageMiningSubsystem: T0 ← GenerateNextTicket(T1.randomness(), StorageMiner.WorkerKey)            
            else Null block last round
                StorageMiningSubsystem ->> StorageMiningSubsystem: T1 ← GenerateNextTicket(T0.randomness(), StorageMiner.WorkerKey)   
                Note Right of StorageMiningSubsystem: Using tickets derived in failed election proof in last epoch
            end
            StorageMiningSubsystem ->>+ StoragePowerConsensusSubsystem: TryLeaderElection(EP)
            StoragePowerConsensusSubsystem -->>- StorageMiningSubsystem: {1, 0}
            opt 1- success
                StorageMiningSubsystem ->> BlockProducer: GenerateBlock(EP, T0, Tipset, StorageMiner.Address)
                BlockProducer ->>+ MessagePool: GetMostProfitableMessages(StorageMiner.Address)
                MessagePool -->>- BlockProducer: [Message]
                BlockProducer ->> BlockProducer: block ← AssembleBlock([Message], Tipset, EP, T0, StorageMiner.Address)
                BlockProducer ->> BlockSyncer: PropagateBlock(block)
            end
        end
    end
    
     opt MiningScheduler
        opt Expired deals
            BlockchainSubsystem ->> SectorIndexerSubsystem: OnNewTipset(Chain, Epoch)
            SectorIndexerSubsystem ->> SectorIndexerSubsystem: [SectorID] ← LookupSectorByDealExpiry(Epoch)
            SectorIndexerSubsystem ->> SectorIndexerSubsystem: PurgeSectorsWithNoLiveDeals([SectorID])
        end
        Note Right of MiningScheduler: Schedule and resume PoSts
        Note Right of MiningScheduler: Schedule and resume SEALs
        Note Right of MiningScheduler: Maintain FaultSet
        Note Right of MiningScheduler: Maintain DoneSet
    end
    
    opt ClockSubsystem
        Note Right of ClockSubsystem: Process expired deals
    end

    opt Storage Fault
        opt Declaration within a Proving Period
            StorageMinerSubsystem ->> StorageMinerActor: UpdateSectorStatus([FaultSet], SectorStateSets)
            StorageMinerActor ->> StoragePowerActor: RecomputeMinerPower()
            Note SectorStateSets := (FaultSet, RecoverSet, ExpireSet)
        end

        opt Miner DID NOT win blocks this proving period -- deadline challenge
            StorageMiningSubsystem ->> StorageMinerActor: SubmitPoSt(PoStProof, SectorStateSets)
        end

        loop EveryBlock
            CronActor ->> StoragePowerActor: VerifyPosts()
            loop forEach StorageMinerActor in StoragePowerActor.Miners
                alt if miner ProvingPeriod ends
                    StoragePowerActor -->>+ StorageMinerActor: ProvingPeriodUpdate()
                    StorageMinerActor ->> StorageMinerActor: computeProvingPeriodEndSectorState()
                    Note Right of StorageMinerActor: FaultSet is all sectors if no post submitted
                    Note Right of StorageMinerActor: sectors Faulted longer than threshold proving periods are destroyed
                    StorageMinerActor ->> StorageMinerActor: UpdateSectorStatus(newSectorState)
                    StorageMinerActor ->> StoragePowerActor: RecomputeMinerPower()
                    StorageMinerActor ->> StorageMarketActor: HandleFailedDeals([newSectorState.DestroyedSet])
                end
            end
        end
    end

    opt Consensus Fault
        StorageMinerActor -->> StoragePowerActor: DeclareConsensusFault(ConsensusFaultProof)
        StoragePowerActor -->+ StoragePowerConsensusSubsystem: ValidateFault(ConsensusFaultProof)

        alt Valid Fault
            StoragePowerConsensusSubsystem -->> StoragePowerActor: TerminateMiner(Address)
            StoragePowerConsensusSubsystem -->> StoragePowerActor: SlashPledgeCollateral(Address)
            StoragePowerConsensusSubsystem -->- StorageMinerActor: UpdateBalance(Reward)
        end
    end
{{% /mermaid %}}