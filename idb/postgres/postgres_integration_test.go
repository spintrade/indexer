package postgres

import (
	//"context"
	"context"
	"database/sql"
	//"fmt"
	//"sync"
	"testing"

	"github.com/algorand/go-algorand/data/basics"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/algorand/indexer/idb"
	"github.com/algorand/indexer/util/test"
)

// TestMaxRoundOnUninitializedDB makes sure we return 0 when getting the max round on a new DB.
func TestMaxRoundOnUninitializedDB(t *testing.T) {
	_, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	///////////
	// Given // A database that has not yet imported the genesis accounts.
	///////////
	db, err := OpenPostgres(connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	//////////
	// When // We request the max round.
	//////////
	roundA, errA := db.GetMaxRoundAccounted()
	roundL, errL := db.getMaxRoundLoaded()

	//////////
	// Then // The error message should be set.
	//////////
	assert.Equal(t, errA, idb.ErrorNotInitialized)
	assert.Equal(t, errL, idb.ErrorNotInitialized)
	assert.Equal(t, uint64(0), roundA)
	assert.Equal(t, uint64(0), roundL)
}

// TestMaxRoundEmptyMetastate makes sure we return 0 when the metastate is empty.
func TestMaxRoundEmptyMetastate(t *testing.T) {
	pg, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()
	///////////
	// Given // The database has the metastate set but the account_round is missing.
	///////////
	db, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)
	pg.Exec(`INSERT INTO metastate (k, v) values ('state', '{}')`)

	//////////
	// When // We request the max round.
	//////////
	round, err := db.GetMaxRoundAccounted()

	//////////
	// Then // The error message should be set.
	//////////
	assert.Equal(t, err, idb.ErrorNotInitialized)
	assert.Equal(t, uint64(0), round)
}

// TestMaxRound the happy path.
func TestMaxRound(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()
	///////////
	// Given // The database has the metastate set normally.
	///////////
	pdb, err := OpenPostgres(connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)
	db.Exec(`INSERT INTO metastate (k, v) values ($1, $2)`, "state", "{\"account_round\":123454321}")
	db.Exec(`INSERT INTO block_header (round, realtime, rewardslevel, header) VALUES ($1, NOW(), 0, '{}') ON CONFLICT DO NOTHING`, 543212345)

	//////////
	// When // We request the max round.
	//////////
	roundA, err := pdb.GetMaxRoundAccounted()
	assert.NoError(t, err)
	roundL, err := pdb.getMaxRoundLoaded()
	assert.NoError(t, err)

	//////////
	// Then // There should be no error and we return that there are zero rounds.
	//////////
	assert.Equal(t, uint64(123454321), roundA)
	assert.Equal(t, uint64(543212345), roundL)
}

func assertAccountAsset(t *testing.T, db *sql.DB, addr basics.Address, assetid uint64, frozen bool, amount uint64) {
	var row *sql.Row
	var f bool
	var a uint64

	row = db.QueryRow(`SELECT frozen, amount FROM account_asset as a WHERE a.addr = $1 AND assetid = $2`, addr[:], assetid)
	err := row.Scan(&f, &a)
	assert.NoError(t, err, "failed looking up AccountA.")
	assert.Equal(t, frozen, f)
	assert.Equal(t, amount, a)
}

// TestAssetCloseReopenTransfer tests a scenario that requires asset subround accounting
func TestAssetCloseReopenTransfer(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	err := db.LoadGenesis(test.MakeGenesis())
	require.NoError(t, err)

	block0 := test.MakeGenesisBlock()
	err = db.AddBlock(block0)
	require.NoError(t, err)

	assetid := uint64(1)
	amt := uint64(10000)
	total := uint64(1000000)

	///////////
	// Given // A round scenario requiring subround accounting: AccountA is funded, closed, opts back, and funded again.
	///////////
	createAsset := test.MakeAssetConfigTxn(
		total, uint64(6), false, "icicles", "frozen coin",
		"http://antarctica.com", test.AccountD)
	optInA := test.MakeAssetOptInTxn(assetid, test.AccountA)
	fundA := test.MakeAssetTransferTxn(
		assetid, amt, 0, test.AccountD, test.AccountA, basics.Address{})
	optInB := test.MakeAssetOptInTxn(assetid, test.AccountB)
	optInC := test.MakeAssetOptInTxn(assetid, test.AccountC)
	closeA := test.MakeAssetTransferTxn(
		assetid, 1000, 0, test.AccountA, test.AccountB, test.AccountC)
	payMain := test.MakeAssetTransferTxn(
		assetid, amt, 0, test.AccountD, test.AccountA, basics.Address{})

	block1 := test.MakeBlockForTxns(
		block0.BlockHeader, &createAsset, &optInA, &fundA, &optInB, &optInC, &closeA,
		&optInA, &payMain)

	//////////
	// When // We commit the block to the database
	//////////
	err = db.AddBlock(block1)
	require.NoError(t, err)

	//////////
	// Then // Accounts A, B, C and D have the correct balances.
	//////////
	// A has the final payment after being closed out
	assertAccountAsset(t, db.db, test.AccountA, assetid, false, amt)
	// B has the closing transfer amount
	assertAccountAsset(t, db.db, test.AccountB, assetid, false, 1000)
	// C has the close-to remainder
	assertAccountAsset(t, db.db, test.AccountC, assetid, false, 9000)
	// D has the total minus both payments to A
	assertAccountAsset(t, db.db, test.AccountD, assetid, false, total-2*amt)
}


// TestReCreateAssetHolding checks the optin value of a defunct
func TestReCreateAssetHolding(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	err := db.LoadGenesis(test.MakeGenesis())
	require.NoError(t, err)

	block := test.MakeGenesisBlock()
	err = db.AddBlock(block)
	require.NoError(t, err)

	total := uint64(1000000)

	for i, frozen := range []bool{true, false} {
		assetid := uint64(1 + 5 * i)
		///////////
		// Given //
		// A new asset with default-frozen, AccountB opts-in and has its frozen state
		// toggled.
		/////////// Then AccountB opts-out then opts-in again.
		createAssetFrozen := test.MakeAssetConfigTxn(
			total, uint64(6), frozen, "icicles", "frozen coin",
			"http://antarctica.com", test.AccountA)
		optinB := test.MakeAssetOptInTxn(assetid, test.AccountB)
		unfreezeB := test.MakeAssetFreezeTxn(
			assetid, !frozen, test.AccountA, test.AccountB)
		optoutB := test.MakeAssetTransferTxn(
			assetid, 0, 0, test.AccountB, test.AccountC, test.AccountD)

		block = test.MakeBlockForTxns(
			block.BlockHeader, &createAssetFrozen, &optinB, &unfreezeB, &optoutB, &optinB)

		//////////
		// When // We commit the round accounting to the database.
		//////////
		err = db.AddBlock(block)
		require.NoError(t, err)

		//////////
		// Then // AccountB should have its frozen state set back to the default value
		//////////
		assertAccountAsset(t, db.db, test.AccountB, assetid, frozen, 0)
	}
}

// TestMultipleAssetOptins make sure no-op transactions don't reset the default frozen value.
func TestNoopOptins(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	err := db.LoadGenesis(test.MakeGenesis())
	require.NoError(t, err)

	block := test.MakeGenesisBlock()
	err = db.AddBlock(block)
	require.NoError(t, err)

	///////////
	// Given //
	// An asset with default-frozen = true, AccountB opts in, is unfrozen, then has a
	// no-op opt-in
	///////////

	assetid := uint64(1)

	createAsset := test.MakeAssetConfigTxn(
		uint64(1000000), uint64(6), true, "icicles", "frozen coin",
		"http://antarctica.com", test.AccountD)
	optinB := test.MakeAssetOptInTxn(assetid, test.AccountB)
	unfreezeB := test.MakeAssetFreezeTxn(assetid, false, test.AccountD, test.AccountB)

	block = test.MakeBlockForTxns(
		block.BlockHeader, &createAsset, &optinB, &unfreezeB, &optinB)

	//////////
	// When // We commit the round accounting to the database.
	//////////
	err = db.AddBlock(block)
	require.NoError(t, err)

	//////////
	// Then // AccountB should have its frozen state set back to the default value
	//////////
	assertAccountAsset(t, db.db, test.AccountB, assetid, false, 0)
}

/*
// TestMultipleWriters tests that accounting cannot be double committed.
func TestMultipleWriters(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	amt := uint64(10000)

	///////////
	// Given // Send amt to AccountA
	///////////
	_, payAccountA := test.MakePayTxnRowOrPanic(test.Round, 1000, amt, 0, 0, 0, 0, test.AccountD,
		test.AccountA, sdk_types.ZeroAddress, sdk_types.ZeroAddress)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)
	state.AddTransaction(payAccountA)

	//////////
	// When // We attempt commit the round accounting multiple times.
	//////////
	start := make(chan struct{})
	commits := 10
	errors := make(chan error, commits)
	var wg sync.WaitGroup
	for i := 0; i < commits; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errors <- pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
		}()
	}
	close(start)

	wg.Wait()
	close(errors)

	//////////
	// Then // There should be num-1 errors, and AccountA should only be paid once.
	//////////
	errorCount := 0
	for err := range errors {
		if err != nil {
			errorCount++
		}
	}
	assert.Equal(t, commits-1, errorCount)

	// AccountA should contain the final payment.
	var balance uint64
	row := db.QueryRow(`SELECT microalgos FROM account WHERE account.addr = $1`, test.AccountA[:])
	err = row.Scan(&balance)
	assert.NoError(t, err, "checking balance")
	assert.Equal(t, amt, balance)
}

// TestBlockWithTransactions tests that the block with transactions endpoint works.
func TestBlockWithTransactions(t *testing.T) {
	var err error

	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	assetid := uint64(2222)
	amt := uint64(10000)
	total := uint64(1000000)

	///////////
	// Given // A block at round test.Round with 5 transactions.
	///////////
	tx1, row1 := test.MakeAssetConfigOrPanic(test.Round, 0, assetid, total, uint64(6), false, "icicles", "frozen coin", "http://antarctica.com", test.AccountD)
	tx2, row2 := test.MakeAssetTxnOrPanic(test.Round, assetid, amt, test.AccountD, test.AccountA, sdk_types.ZeroAddress)
	tx3, row3 := test.MakeAssetTxnOrPanic(test.Round, assetid, 1000, test.AccountA, test.AccountB, test.AccountC)
	tx4, row4 := test.MakeAssetTxnOrPanic(test.Round, assetid, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress)
	tx5, row5 := test.MakeAssetTxnOrPanic(test.Round, assetid, amt, test.AccountD, test.AccountA, sdk_types.ZeroAddress)
	txns := []*sdk_types.SignedTxnWithAD{tx1, tx2, tx3, tx4, tx5}
	txnRows := []*idb.TxnRow{row1, row2, row3, row4, row5}

	_, err = db.Exec(`INSERT INTO metastate (k, v) values ($1, $2)`, "state", `{"account_round": 11}`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO block_header (round, realtime, rewardslevel, header) VALUES ($1, NOW(), 0, '{}') ON CONFLICT DO NOTHING`, test.Round)
	require.NoError(t, err)
	for i := range txns {
		_, err = db.Exec(`INSERT INTO txn (round, intra, typeenum, asset, txid, txnbytes, txn) VALUES ($1, $2, $3, $4, $5, $6, $7)`, test.Round, i, 0, 0, crypto.TransactionID(txns[i].Txn), txnRows[i].TxnBytes, "{}")
		require.NoError(t, err)
	}

	//////////
	// When // We call GetBlock and Transactions
	//////////
	_, blockTxn, err := pdb.GetBlock(context.Background(), test.Round, idb.GetBlockOptions{Transactions: true})
	require.NoError(t, err)
	round := test.Round
	txnRow, _ := pdb.Transactions(context.Background(), idb.TransactionFilter{Round: &round})
	transactionsTxn := make([]idb.TxnRow, 0)
	for row := range txnRow {
		require.NoError(t, row.Error)
		transactionsTxn = append(transactionsTxn, row)
	}

	//////////
	// Then // They should have the same transactions
	//////////
	assert.Len(t, blockTxn, 5)
	assert.Len(t, transactionsTxn, 5)
	for i := 0; i < len(blockTxn); i++ {
		assert.Equal(t, txnRows[i].TxnBytes, blockTxn[i].TxnBytes)
		assert.Equal(t, txnRows[i].TxnBytes, transactionsTxn[i].TxnBytes)
	}
}

func TestRekeyBasic(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	///////////
	// Given // Send rekey transaction
	///////////
	_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
		test.AccountA, sdk_types.ZeroAddress, test.AccountB)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)
	state.AddTransaction(txnRow)

	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	//////////
	// Then // Account A is rekeyed to account B
	//////////
	var accountDataStr []byte
	row := db.QueryRow(`SELECT account_data FROM account WHERE account.addr = $1`, test.AccountA[:])
	err = row.Scan(&accountDataStr)
	assert.NoError(t, err, "querying account data")

	var ad types.AccountData
	err = encoding.DecodeJSON(accountDataStr, &ad)
	assert.NoError(t, err, "failed to parse account data json")
	assert.Equal(t, test.AccountB, ad.SpendingKey)
}

func TestRekeyToItself(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	///////////
	// Given // Send rekey transaction
	///////////
	{
		_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
			test.AccountA, sdk_types.ZeroAddress, test.AccountB)

		cache, err := pdb.GetDefaultFrozen()
		assert.NoError(t, err)
		state := getAccounting(test.Round, cache)
		state.AddTransaction(txnRow)

		err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
		assert.NoError(t, err, "failed to commit")
	}
	{
		_, txnRow := test.MakePayTxnRowOrPanic(test.Round+1, 1000, 0, 0, 0, 0, 0, test.AccountA,
			test.AccountA, sdk_types.ZeroAddress, test.AccountA)

		cache, err := pdb.GetDefaultFrozen()
		assert.NoError(t, err)
		state := getAccounting(test.Round+1, cache)
		state.AddTransaction(txnRow)

		err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round+1, &types.Block{})
		assert.NoError(t, err, "failed to commit")
	}

	//////////
	// Then // Account's A auth-address is not recorded
	//////////
	var accountDataStr []byte
	row := db.QueryRow(`SELECT account_data FROM account WHERE account.addr = $1`, test.AccountA[:])
	err = row.Scan(&accountDataStr)
	assert.NoError(t, err, "querying account data")

	var ad types.AccountData
	err = encoding.DecodeJSON(accountDataStr, &ad)
	assert.NoError(t, err, "failed to parse account data json")
	assert.Equal(t, sdk_types.ZeroAddress, ad.SpendingKey)
}

func TestRekeyThreeTimesInSameRound(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	///////////
	// Given // Send rekey transaction
	///////////
	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)

	{
		_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
			test.AccountA, sdk_types.ZeroAddress, test.AccountB)
		state.AddTransaction(txnRow)
	}
	{
		_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
			test.AccountA, sdk_types.ZeroAddress, sdk_types.ZeroAddress)
		state.AddTransaction(txnRow)
	}
	{
		_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
			test.AccountA, sdk_types.ZeroAddress, test.AccountC)
		state.AddTransaction(txnRow)
	}

	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	//////////
	// Then // Account A is rekeyed to account C
	//////////
	var accountDataStr []byte
	row := db.QueryRow(`SELECT account_data FROM account WHERE account.addr = $1`, test.AccountA[:])
	err = row.Scan(&accountDataStr)
	assert.NoError(t, err, "querying account data")

	var ad types.AccountData
	err = encoding.DecodeJSON(accountDataStr, &ad)
	assert.NoError(t, err, "failed to parse account data json")
	assert.Equal(t, test.AccountC, ad.SpendingKey)
}

func TestRekeyToItselfHasNotBeenRekeyed(t *testing.T) {
	_, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	///////////
	// Given // Send rekey transaction
	///////////
	_, txnRow := test.MakePayTxnRowOrPanic(test.Round, 1000, 0, 0, 0, 0, 0, test.AccountA,
		test.AccountA, sdk_types.ZeroAddress, sdk_types.ZeroAddress)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)
	state.AddTransaction(txnRow)

	//////////
	// Then // No error when committing to the DB.
	//////////
	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")
}

// TestIgnoreDefaultFrozenConfigUpdate the creator asset holding should ignore default-frozen = true.
func TestIgnoreDefaultFrozenConfigUpdate(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	assetid := uint64(2222)
	total := uint64(1000000)

	///////////
	// Given // A new asset with default-frozen = true, and AccountB opting into it.
	///////////
	_, createAssetNotFrozen := test.MakeAssetConfigOrPanic(test.Round, 0, assetid, total, uint64(6), false, "icicles", "frozen coin", "http://antarctica.com", test.AccountA)
	_, modifyAssetToFrozen := test.MakeAssetConfigOrPanic(test.Round, assetid, assetid, total, uint64(6), true, "icicles", "frozen coin", "http://antarctica.com", test.AccountA)
	_, optin := test.MakeAssetTxnOrPanic(test.Round, assetid, 0, test.AccountB, test.AccountB, sdk_types.ZeroAddress)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)
	state.AddTransaction(createAssetNotFrozen)
	state.AddTransaction(modifyAssetToFrozen)
	state.AddTransaction(optin)

	//////////
	// When // We commit the round accounting to the database.
	//////////
	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	//////////
	// Then // Make sure the accounts have the correct default-frozen after create/optin
	//////////
	// default-frozen = true
	assertAccountAsset(t, db, test.AccountA, assetid, false, total)
	assertAccountAsset(t, db, test.AccountB, assetid, false, 0)
}

// TestZeroTotalAssetCreate tests that the asset holding with total of 0 is created.
func TestZeroTotalAssetCreate(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	assetid := uint64(2222)
	total := uint64(0)

	///////////
	// Given // A new asset with total = 0.
	///////////
	_, createAsset := test.MakeAssetConfigOrPanic(test.Round, 0, assetid, total, uint64(6), false, "icicles", "frozen coin", "http://antarctica.com", test.AccountA)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)
	state := getAccounting(test.Round, cache)
	state.AddTransaction(createAsset)

	//////////
	// When // We commit the round accounting to the database.
	//////////
	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	//////////
	// Then // Make sure the creator has an asset holding with amount = 0.
	//////////
	assertAccountAsset(t, db, test.AccountA, assetid, false, 0)
}

func assertAssetDates(t *testing.T, db *sql.DB, assetID uint64, deleted sql.NullBool, createdAt sql.NullInt64, closedAt sql.NullInt64) {
	row := db.QueryRow(
		"SELECT deleted, created_at, closed_at FROM asset WHERE index = $1", int64(assetID))

	var retDeleted sql.NullBool
	var retCreatedAt sql.NullInt64
	var retClosedAt sql.NullInt64
	err := row.Scan(&retDeleted, &retCreatedAt, &retClosedAt)
	assert.NoError(t, err)

	assert.Equal(t, deleted, retDeleted)
	assert.Equal(t, createdAt, retCreatedAt)
	assert.Equal(t, closedAt, retClosedAt)
}

func assertAssetHoldingDates(t *testing.T, db *sql.DB, address sdk_types.Address, assetID uint64, deleted sql.NullBool, createdAt sql.NullInt64, closedAt sql.NullInt64) {
	row := db.QueryRow(
		"SELECT deleted, created_at, closed_at FROM account_asset WHERE "+
			"addr = $1 AND assetid = $2",
		address[:], assetID)

	var retDeleted sql.NullBool
	var retCreatedAt sql.NullInt64
	var retClosedAt sql.NullInt64
	err := row.Scan(&retDeleted, &retCreatedAt, &retClosedAt)
	assert.NoError(t, err)

	assert.Equal(t, deleted, retDeleted)
	assert.Equal(t, createdAt, retCreatedAt)
	assert.Equal(t, closedAt, retClosedAt)
}

func TestDestroyAssetBasic(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)

	assetID := uint64(3)

	// Create an asset.
	{
		_, txnRow := test.MakeAssetConfigOrPanic(test.Round, 0, assetID, 4, 0, false, "uu", "aa", "",
			test.AccountA)

		state := getAccounting(test.Round, cache)
		err := state.AddTransaction(txnRow)
		assert.NoError(t, err)

		err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
		assert.NoError(t, err, "failed to commit")
	}
	// Destroy an asset.
	{
		_, txnRow := test.MakeAssetDestroyTxn(test.Round+1, assetID)

		state := getAccounting(test.Round+1, cache)
		err := state.AddTransaction(txnRow)
		assert.NoError(t, err)

		err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round+1, &types.Block{})
		assert.NoError(t, err, "failed to commit")
	}

	// Check that the asset is deleted.
	assertAssetDates(t, db, assetID,
		sql.NullBool{Valid: true, Bool: true},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: true, Int64: int64(test.Round + 1)})

	// Check that the account's asset holding is deleted.
	assertAssetHoldingDates(t, db, test.AccountA, assetID,
		sql.NullBool{Valid: true, Bool: true},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: true, Int64: int64(test.Round + 1)})
}

func TestDestroyAssetZeroSupply(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)

	assetID := uint64(3)

	state := getAccounting(test.Round, cache)

	// Create an asset.
	{
		// Set total supply to 0.
		_, txnRow := test.MakeAssetConfigOrPanic(test.Round, 0, assetID, 0, 0, false, "uu", "aa", "",
			test.AccountA)

		err := state.AddTransaction(txnRow)
		assert.NoError(t, err)
	}
	// Destroy an asset.
	{
		_, txnRow := test.MakeAssetDestroyTxn(test.Round, assetID)

		err := state.AddTransaction(txnRow)
		assert.NoError(t, err)
	}

	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	// Check that the asset is deleted.
	assertAssetDates(t, db, assetID,
		sql.NullBool{Valid: true, Bool: true},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)})

	// Check that the account's asset holding is deleted.
	assertAssetHoldingDates(t, db, test.AccountA, assetID,
		sql.NullBool{Valid: true, Bool: true},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)})
}

func TestDestroyAssetDeleteCreatorsHolding(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()

	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)

	cache, err := pdb.GetDefaultFrozen()
	assert.NoError(t, err)

	assetID := uint64(3)

	state := getAccounting(test.Round, cache)

	// Create an asset.
	{
		// Create a transaction where all special addresses are different from creator's address.
		txn := sdk_types.SignedTxnWithAD{
			SignedTxn: sdk_types.SignedTxn{
				Txn: sdk_types.Transaction{
					Type: "acfg",
					Header: sdk_types.Header{
						Sender: test.AccountA,
					},
					AssetConfigTxnFields: sdk_types.AssetConfigTxnFields{
						AssetParams: sdk_types.AssetParams{
							Manager:  test.AccountB,
							Reserve:  test.AccountB,
							Freeze:   test.AccountB,
							Clawback: test.AccountB,
						},
					},
				},
			},
		}
		txnRow := idb.TxnRow{
			Round:    uint64(test.Round),
			TxnBytes: msgpack.Encode(txn),
			AssetID:  assetID,
		}

		err := state.AddTransaction(&txnRow)
		assert.NoError(t, err)
	}
	// Another account opts in.
	{
		_, txnRow := test.MakeAssetTxnOrPanic(test.Round, assetID, 0, test.AccountC,
			test.AccountC, sdk_types.ZeroAddress)
		state.AddTransaction(txnRow)
	}
	// Destroy an asset.
	{
		_, txnRow := test.MakeAssetDestroyTxn(test.Round, assetID)
		state.AddTransaction(txnRow)
	}

	err = pdb.CommitRoundAccounting(state.RoundUpdates, test.Round, &types.Block{})
	assert.NoError(t, err, "failed to commit")

	// Check that the creator's asset holding is deleted.
	assertAssetHoldingDates(t, db, test.AccountA, assetID,
		sql.NullBool{Valid: true, Bool: true},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)})

	// Check that other account's asset holding was not deleted.
	assertAssetHoldingDates(t, db, test.AccountC, assetID,
		sql.NullBool{Valid: true, Bool: false},
		sql.NullInt64{Valid: true, Int64: int64(test.Round)},
		sql.NullInt64{Valid: false, Int64: 0})

	// Check that the manager does not have an asset holding.
	{
		count := queryInt(db, "SELECT COUNT(*) FROM account_asset WHERE addr = $1", test.AccountB[:])
		assert.Equal(t, 0, count)
	}
}

// Test that block import adds the freeze/sender accounts to txn_participation.
func TestAssetFreezeTxnParticipation(t *testing.T) {
	db, connStr, shutdownFunc := setupPostgres(t)
	defer shutdownFunc()
	pdb, err := idb.IndexerDbByName("postgres", connStr, idb.IndexerDbOptions{}, nil)
	assert.NoError(t, err)
	blockImporter := importer.NewDBImporter(pdb)

	///////////
	// Given // A block containing an asset freeze txn
	///////////

	// Create a block with freeze txn
	freeze, _ := test.MakeAssetFreezeOrPanic(test.Round, 1234, true, test.AccountA, test.AccountB)
	block := test.MakeBlockForTxns(test.Round, freeze)

	//////////
	// When // We import the block.
	//////////
	txnCount, err := blockImporter.ImportDecodedBlock(&block)
	assert.NoError(t, err, "failed to import")
	assert.Equal(t, 1, txnCount)

	//////////
	// Then // Both accounts should have an entry in the txn_participation table.
	//////////
	acctACount := queryInt(db, "SELECT COUNT(*) FROM txn_participation WHERE addr = $1", test.AccountA[:])
	acctBCount := queryInt(db, "SELECT COUNT(*) FROM txn_participation WHERE addr = $1", test.AccountB[:])
	assert.Equal(t, 1, acctACount)
	assert.Equal(t, 1, acctBCount)
}
*/

// TestAddBlockGenesis tests that adding block 0 is successful.
func TestAddBlockGenesis(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	err := db.LoadGenesis(test.MakeGenesis())
	require.NoError(t, err)

	block := test.MakeGenesisBlock()

	err = db.AddBlock(block)
	require.NoError(t, err)

	opts := idb.GetBlockOptions{
		Transactions: true,
	}
	blockRet, txns, err := db.GetBlock(context.Background(), 0, opts)
	require.NoError(t, err)
	assert.Empty(t, txns)
	assert.Equal(t, block, blockRet)

	maxRound, err := db.GetMaxRoundAccounted()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), maxRound)
}
