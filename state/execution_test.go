package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/crypto"
	"github.com/tendermint/tendermint/crypto/ed25519"
	cryptoenc "github.com/tendermint/tendermint/crypto/encoding"
	"github.com/tendermint/tendermint/crypto/tmhash"
	mmock "github.com/tendermint/tendermint/internal/mempool/mock"
	"github.com/tendermint/tendermint/libs/log"
	tmtime "github.com/tendermint/tendermint/libs/time"
	"github.com/tendermint/tendermint/pkg/abci"
	bl "github.com/tendermint/tendermint/pkg/block"
	"github.com/tendermint/tendermint/pkg/consensus"
	"github.com/tendermint/tendermint/pkg/events"
	"github.com/tendermint/tendermint/pkg/evidence"
	"github.com/tendermint/tendermint/pkg/light"
	"github.com/tendermint/tendermint/pkg/metadata"
	"github.com/tendermint/tendermint/proxy"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/state/mocks"
	sf "github.com/tendermint/tendermint/state/test/factory"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/version"
	dbm "github.com/tendermint/tm-db"
)

var (
	chainID             = "execution_chain"
	testPartSize uint32 = 65536
)

func TestApplyBlock(t *testing.T) {
	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	state, stateDB, _ := makeState(1, 1)
	stateStore := sm.NewStore(stateDB)
	blockStore := store.NewBlockStore(dbm.NewMemDB())
	blockExec := sm.NewBlockExecutor(stateStore, log.TestingLogger(), proxyApp.Consensus(),
		mmock.Mempool{}, sm.EmptyEvidencePool{}, blockStore)

	block := sf.MakeBlock(state, 1, new(metadata.Commit))
	blockID := metadata.BlockID{Hash: block.Hash(), PartSetHeader: block.MakePartSet(testPartSize).Header()}

	state, err = blockExec.ApplyBlock(state, blockID, block)
	require.Nil(t, err)

	// TODO check state and mempool
	assert.EqualValues(t, 1, state.Version.Consensus.App, "App version wasn't updated")
}

// TestBeginBlockValidators ensures we send absent validators list.
func TestBeginBlockValidators(t *testing.T) {
	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // no need to check error again

	state, stateDB, _ := makeState(2, 2)
	stateStore := sm.NewStore(stateDB)

	prevHash := state.LastBlockID.Hash
	prevParts := metadata.PartSetHeader{}
	prevBlockID := metadata.BlockID{Hash: prevHash, PartSetHeader: prevParts}

	var (
		now        = tmtime.Now()
		commitSig0 = metadata.NewCommitSigForBlock(
			[]byte("Signature1"),
			state.Validators.Validators[0].Address,
			now)
		commitSig1 = metadata.NewCommitSigForBlock(
			[]byte("Signature2"),
			state.Validators.Validators[1].Address,
			now)
		absentSig = metadata.NewCommitSigAbsent()
	)

	testCases := []struct {
		desc                     string
		lastCommitSigs           []metadata.CommitSig
		expectedAbsentValidators []int
	}{
		{"none absent", []metadata.CommitSig{commitSig0, commitSig1}, []int{}},
		{"one absent", []metadata.CommitSig{commitSig0, absentSig}, []int{1}},
		{"multiple absent", []metadata.CommitSig{absentSig, absentSig}, []int{0, 1}},
	}

	for _, tc := range testCases {
		lastCommit := metadata.NewCommit(1, 0, prevBlockID, tc.lastCommitSigs)

		// block for height 2
		block := sf.MakeBlock(state, 2, lastCommit)

		_, err = sm.ExecCommitBlock(nil, proxyApp.Consensus(), block, log.TestingLogger(), stateStore, 1, state)
		require.Nil(t, err, tc.desc)

		// -> app receives a list of validators with a bool indicating if they signed
		ctr := 0
		for i, v := range app.CommitVotes {
			if ctr < len(tc.expectedAbsentValidators) &&
				tc.expectedAbsentValidators[ctr] == i {

				assert.False(t, v.SignedLastBlock)
				ctr++
			} else {
				assert.True(t, v.SignedLastBlock)
			}
		}
	}
}

// TestBeginBlockByzantineValidators ensures we send byzantine validators list.
func TestBeginBlockByzantineValidators(t *testing.T) {
	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	state, stateDB, privVals := makeState(1, 1)
	stateStore := sm.NewStore(stateDB)

	defaultEvidenceTime := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	privVal := privVals[state.Validators.Validators[0].Address.String()]
	blockID := makeBlockID([]byte("headerhash"), 1000, []byte("partshash"))
	header := &metadata.Header{
		Version:            version.Consensus{Block: version.BlockProtocol, App: 1},
		ChainID:            state.ChainID,
		Height:             10,
		Time:               defaultEvidenceTime,
		LastBlockID:        blockID,
		LastCommitHash:     crypto.CRandBytes(tmhash.Size),
		DataHash:           crypto.CRandBytes(tmhash.Size),
		ValidatorsHash:     state.Validators.Hash(),
		NextValidatorsHash: state.Validators.Hash(),
		ConsensusHash:      crypto.CRandBytes(tmhash.Size),
		AppHash:            crypto.CRandBytes(tmhash.Size),
		LastResultsHash:    crypto.CRandBytes(tmhash.Size),
		EvidenceHash:       crypto.CRandBytes(tmhash.Size),
		ProposerAddress:    crypto.CRandBytes(crypto.AddressSize),
	}

	// we don't need to worry about validating the evidence as long as they pass validate basic
	dve := evidence.NewMockDuplicateVoteEvidenceWithValidator(3, defaultEvidenceTime, privVal, state.ChainID)
	dve.ValidatorPower = 1000
	lcae := &evidence.LightClientAttackEvidence{
		ConflictingBlock: &light.LightBlock{
			SignedHeader: &metadata.SignedHeader{
				Header: header,
				Commit: metadata.NewCommit(10, 0, makeBlockID(header.Hash(), 100, []byte("partshash")), []metadata.CommitSig{{
					BlockIDFlag:      metadata.BlockIDFlagNil,
					ValidatorAddress: crypto.AddressHash([]byte("validator_address")),
					Timestamp:        defaultEvidenceTime,
					Signature:        crypto.CRandBytes(metadata.MaxSignatureSize),
				}}),
			},
			ValidatorSet: state.Validators,
		},
		CommonHeight:        8,
		ByzantineValidators: []*consensus.Validator{state.Validators.Validators[0]},
		TotalVotingPower:    12,
		Timestamp:           defaultEvidenceTime,
	}

	ev := []evidence.Evidence{dve, lcae}

	abciEv := []abci.Evidence{
		{
			Type:             abci.EvidenceType_DUPLICATE_VOTE,
			Height:           3,
			Time:             defaultEvidenceTime,
			Validator:        consensus.TM2PB.Validator(state.Validators.Validators[0]),
			TotalVotingPower: 10,
		},
		{
			Type:             abci.EvidenceType_LIGHT_CLIENT_ATTACK,
			Height:           8,
			Time:             defaultEvidenceTime,
			Validator:        consensus.TM2PB.Validator(state.Validators.Validators[0]),
			TotalVotingPower: 12,
		},
	}

	evpool := &mocks.EvidencePool{}
	evpool.On("PendingEvidence", mock.AnythingOfType("int64")).Return(ev, int64(100))
	evpool.On("Update", mock.AnythingOfType("state.State"), mock.AnythingOfType("evidence.EvidenceList")).Return()
	evpool.On("CheckEvidence", mock.AnythingOfType("evidence.EvidenceList")).Return(nil)

	blockStore := store.NewBlockStore(dbm.NewMemDB())

	blockExec := sm.NewBlockExecutor(stateStore, log.TestingLogger(), proxyApp.Consensus(),
		mmock.Mempool{}, evpool, blockStore)

	block := sf.MakeBlock(state, 1, new(metadata.Commit))
	block.Evidence = bl.EvidenceData{Evidence: ev}
	block.Header.EvidenceHash = block.Evidence.Hash()
	blockID = metadata.BlockID{Hash: block.Hash(), PartSetHeader: block.MakePartSet(testPartSize).Header()}

	_, err = blockExec.ApplyBlock(state, blockID, block)
	require.Nil(t, err)

	// TODO check state and mempool
	assert.Equal(t, abciEv, app.ByzantineValidators)
}

func TestValidateValidatorUpdates(t *testing.T) {
	pubkey1 := ed25519.GenPrivKey().PubKey()
	pubkey2 := ed25519.GenPrivKey().PubKey()
	pk1, err := cryptoenc.PubKeyToProto(pubkey1)
	assert.NoError(t, err)
	pk2, err := cryptoenc.PubKeyToProto(pubkey2)
	assert.NoError(t, err)

	defaultValidatorParams := consensus.ValidatorParams{PubKeyTypes: []string{consensus.ABCIPubKeyTypeEd25519}}

	testCases := []struct {
		name string

		abciUpdates     []abci.ValidatorUpdate
		validatorParams consensus.ValidatorParams

		shouldErr bool
	}{
		{
			"adding a validator is OK",
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: 20}},
			defaultValidatorParams,
			false,
		},
		{
			"updating a validator is OK",
			[]abci.ValidatorUpdate{{PubKey: pk1, Power: 20}},
			defaultValidatorParams,
			false,
		},
		{
			"removing a validator is OK",
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: 0}},
			defaultValidatorParams,
			false,
		},
		{
			"adding a validator with negative power results in error",
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: -100}},
			defaultValidatorParams,
			true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := sm.ValidateValidatorUpdates(tc.abciUpdates, tc.validatorParams)
			if tc.shouldErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUpdateValidators(t *testing.T) {
	pubkey1 := ed25519.GenPrivKey().PubKey()
	val1 := consensus.NewValidator(pubkey1, 10)
	pubkey2 := ed25519.GenPrivKey().PubKey()
	val2 := consensus.NewValidator(pubkey2, 20)

	pk, err := cryptoenc.PubKeyToProto(pubkey1)
	require.NoError(t, err)
	pk2, err := cryptoenc.PubKeyToProto(pubkey2)
	require.NoError(t, err)

	testCases := []struct {
		name string

		currentSet  *consensus.ValidatorSet
		abciUpdates []abci.ValidatorUpdate

		resultingSet *consensus.ValidatorSet
		shouldErr    bool
	}{
		{
			"adding a validator is OK",
			consensus.NewValidatorSet([]*consensus.Validator{val1}),
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: 20}},
			consensus.NewValidatorSet([]*consensus.Validator{val1, val2}),
			false,
		},
		{
			"updating a validator is OK",
			consensus.NewValidatorSet([]*consensus.Validator{val1}),
			[]abci.ValidatorUpdate{{PubKey: pk, Power: 20}},
			consensus.NewValidatorSet([]*consensus.Validator{consensus.NewValidator(pubkey1, 20)}),
			false,
		},
		{
			"removing a validator is OK",
			consensus.NewValidatorSet([]*consensus.Validator{val1, val2}),
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: 0}},
			consensus.NewValidatorSet([]*consensus.Validator{val1}),
			false,
		},
		{
			"removing a non-existing validator results in error",
			consensus.NewValidatorSet([]*consensus.Validator{val1}),
			[]abci.ValidatorUpdate{{PubKey: pk2, Power: 0}},
			consensus.NewValidatorSet([]*consensus.Validator{val1}),
			true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			updates, err := consensus.PB2TM.ValidatorUpdates(tc.abciUpdates)
			assert.NoError(t, err)
			err = tc.currentSet.UpdateWithChangeSet(updates)
			if tc.shouldErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				require.Equal(t, tc.resultingSet.Size(), tc.currentSet.Size())

				assert.Equal(t, tc.resultingSet.TotalVotingPower(), tc.currentSet.TotalVotingPower())

				assert.Equal(t, tc.resultingSet.Validators[0].Address, tc.currentSet.Validators[0].Address)
				if tc.resultingSet.Size() > 1 {
					assert.Equal(t, tc.resultingSet.Validators[1].Address, tc.currentSet.Validators[1].Address)
				}
			}
		})
	}
}

// TestEndBlockValidatorUpdates ensures we update validator set and send an event.
func TestEndBlockValidatorUpdates(t *testing.T) {
	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	state, stateDB, _ := makeState(1, 1)
	stateStore := sm.NewStore(stateDB)
	blockStore := store.NewBlockStore(dbm.NewMemDB())

	blockExec := sm.NewBlockExecutor(
		stateStore,
		log.TestingLogger(),
		proxyApp.Consensus(),
		mmock.Mempool{},
		sm.EmptyEvidencePool{},
		blockStore,
	)

	eventBus := events.NewEventBus()
	err = eventBus.Start()
	require.NoError(t, err)
	defer eventBus.Stop() //nolint:errcheck // ignore for tests

	blockExec.SetEventBus(eventBus)

	updatesSub, err := eventBus.Subscribe(
		context.Background(),
		"TestEndBlockValidatorUpdates",
		events.EventQueryValidatorSetUpdates,
	)
	require.NoError(t, err)

	block := sf.MakeBlock(state, 1, new(metadata.Commit))
	blockID := metadata.BlockID{Hash: block.Hash(), PartSetHeader: block.MakePartSet(testPartSize).Header()}

	pubkey := ed25519.GenPrivKey().PubKey()
	pk, err := cryptoenc.PubKeyToProto(pubkey)
	require.NoError(t, err)
	app.ValidatorUpdates = []abci.ValidatorUpdate{
		{PubKey: pk, Power: 10},
	}

	state, err = blockExec.ApplyBlock(state, blockID, block)
	require.Nil(t, err)
	// test new validator was added to NextValidators
	if assert.Equal(t, state.Validators.Size()+1, state.NextValidators.Size()) {
		idx, _ := state.NextValidators.GetByAddress(pubkey.Address())
		if idx < 0 {
			t.Fatalf("can't find address %v in the set %v", pubkey.Address(), state.NextValidators)
		}
	}

	// test we threw an event
	select {
	case msg := <-updatesSub.Out():
		event, ok := msg.Data().(events.EventDataValidatorSetUpdates)
		require.True(t, ok, "Expected event of type EventDataValidatorSetUpdates, got %T", msg.Data())
		if assert.NotEmpty(t, event.ValidatorUpdates) {
			assert.Equal(t, pubkey, event.ValidatorUpdates[0].PubKey)
			assert.EqualValues(t, 10, event.ValidatorUpdates[0].VotingPower)
		}
	case <-updatesSub.Canceled():
		t.Fatalf("updatesSub was canceled (reason: %v)", updatesSub.Err())
	case <-time.After(1 * time.Second):
		t.Fatal("Did not receive EventValidatorSetUpdates within 1 sec.")
	}
}

// TestEndBlockValidatorUpdatesResultingInEmptySet checks that processing validator updates that
// would result in empty set causes no panic, an error is raised and NextValidators is not updated
func TestEndBlockValidatorUpdatesResultingInEmptySet(t *testing.T) {
	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	require.Nil(t, err)
	defer proxyApp.Stop() //nolint:errcheck // ignore for tests

	state, stateDB, _ := makeState(1, 1)
	stateStore := sm.NewStore(stateDB)
	blockStore := store.NewBlockStore(dbm.NewMemDB())
	blockExec := sm.NewBlockExecutor(
		stateStore,
		log.TestingLogger(),
		proxyApp.Consensus(),
		mmock.Mempool{},
		sm.EmptyEvidencePool{},
		blockStore,
	)

	block := sf.MakeBlock(state, 1, new(metadata.Commit))
	blockID := metadata.BlockID{Hash: block.Hash(), PartSetHeader: block.MakePartSet(testPartSize).Header()}

	vp, err := cryptoenc.PubKeyToProto(state.Validators.Validators[0].PubKey)
	require.NoError(t, err)
	// Remove the only validator
	app.ValidatorUpdates = []abci.ValidatorUpdate{
		{PubKey: vp, Power: 0},
	}

	assert.NotPanics(t, func() { state, err = blockExec.ApplyBlock(state, blockID, block) })
	assert.NotNil(t, err)
	assert.NotEmpty(t, state.NextValidators.Validators)
}

func makeBlockID(hash []byte, partSetSize uint32, partSetHash []byte) metadata.BlockID {
	var (
		h   = make([]byte, tmhash.Size)
		psH = make([]byte, tmhash.Size)
	)
	copy(h, hash)
	copy(psH, partSetHash)
	return metadata.BlockID{
		Hash: h,
		PartSetHeader: metadata.PartSetHeader{
			Total: partSetSize,
			Hash:  psH,
		},
	}
}
