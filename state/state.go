package state

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/tendermint/tendermint/account"
	"github.com/tendermint/tendermint/binary"
	"github.com/tendermint/tendermint/block"
	. "github.com/tendermint/tendermint/common"
	dbm "github.com/tendermint/tendermint/db"
	"github.com/tendermint/tendermint/merkle"
)

var (
	stateKey                     = []byte("stateKey")
	minBondAmount                = uint64(1)           // TODO adjust
	defaultAccountsCacheCapacity = 1000                // TODO adjust
	unbondingPeriodBlocks        = uint(60 * 24 * 365) // TODO probably better to make it time based.
	validatorTimeoutBlocks       = uint(10)            // TODO adjust
)

//-----------------------------------------------------------------------------

type InvalidTxError struct {
	Tx     block.Tx
	Reason error
}

func (txErr InvalidTxError) Error() string {
	return fmt.Sprintf("Invalid tx: [%v] reason: [%v]", txErr.Tx, txErr.Reason)
}

//-----------------------------------------------------------------------------

// NOTE: not goroutine-safe.
type State struct {
	DB                  dbm.DB
	LastBlockHeight     uint
	LastBlockHash       []byte
	LastBlockParts      block.PartSetHeader
	LastBlockTime       time.Time
	BondedValidators    *ValidatorSet
	UnbondingValidators *ValidatorSet
	accounts            merkle.Tree // Shouldn't be accessed directly.
	validatorInfos      merkle.Tree // Shouldn't be accessed directly.
}

func LoadState(db dbm.DB) *State {
	s := &State{DB: db}
	buf := db.Get(stateKey)
	if len(buf) == 0 {
		return nil
	} else {
		r, n, err := bytes.NewReader(buf), new(int64), new(error)
		s.LastBlockHeight = binary.ReadUvarint(r, n, err)
		s.LastBlockHash = binary.ReadByteSlice(r, n, err)
		s.LastBlockParts = binary.ReadBinary(block.PartSetHeader{}, r, n, err).(block.PartSetHeader)
		s.LastBlockTime = binary.ReadTime(r, n, err)
		s.BondedValidators = binary.ReadBinary(&ValidatorSet{}, r, n, err).(*ValidatorSet)
		s.UnbondingValidators = binary.ReadBinary(&ValidatorSet{}, r, n, err).(*ValidatorSet)
		accountsHash := binary.ReadByteSlice(r, n, err)
		s.accounts = merkle.NewIAVLTree(binary.BasicCodec, account.AccountCodec, defaultAccountsCacheCapacity, db)
		s.accounts.Load(accountsHash)
		validatorInfosHash := binary.ReadByteSlice(r, n, err)
		s.validatorInfos = merkle.NewIAVLTree(binary.BasicCodec, ValidatorInfoCodec, 0, db)
		s.validatorInfos.Load(validatorInfosHash)
		if *err != nil {
			panic(*err)
		}
		// TODO: ensure that buf is completely read.
	}
	return s
}

// Save this state into the db.
func (s *State) Save() {
	s.accounts.Save()
	s.validatorInfos.Save()
	buf, n, err := new(bytes.Buffer), new(int64), new(error)
	binary.WriteUvarint(s.LastBlockHeight, buf, n, err)
	binary.WriteByteSlice(s.LastBlockHash, buf, n, err)
	binary.WriteBinary(s.LastBlockParts, buf, n, err)
	binary.WriteTime(s.LastBlockTime, buf, n, err)
	binary.WriteBinary(s.BondedValidators, buf, n, err)
	binary.WriteBinary(s.UnbondingValidators, buf, n, err)
	binary.WriteByteSlice(s.accounts.Hash(), buf, n, err)
	binary.WriteByteSlice(s.validatorInfos.Hash(), buf, n, err)
	if *err != nil {
		panic(*err)
	}
	s.DB.Set(stateKey, buf.Bytes())
}

func (s *State) Copy() *State {
	return &State{
		DB:                  s.DB,
		LastBlockHeight:     s.LastBlockHeight,
		LastBlockHash:       s.LastBlockHash,
		LastBlockParts:      s.LastBlockParts,
		LastBlockTime:       s.LastBlockTime,
		BondedValidators:    s.BondedValidators.Copy(),
		UnbondingValidators: s.UnbondingValidators.Copy(),
		accounts:            s.accounts.Copy(),
		validatorInfos:      s.validatorInfos.Copy(),
	}
}

// The accounts from the TxInputs must either already have
// account.PubKey.(type) != PubKeyNil, (it must be known),
// or it must be specified in the TxInput.  If redeclared,
// the TxInput is modified and input.PubKey set to PubKeyNil.
func (s *State) GetOrMakeAccounts(ins []*block.TxInput, outs []*block.TxOutput) (map[string]*account.Account, error) {
	accounts := map[string]*account.Account{}
	for _, in := range ins {
		// Account shouldn't be duplicated
		if _, ok := accounts[string(in.Address)]; ok {
			return nil, block.ErrTxDuplicateAddress
		}
		acc := s.GetAccount(in.Address)
		if acc == nil {
			return nil, block.ErrTxInvalidAddress
		}
		// PubKey should be present in either "account" or "in"
		if _, isNil := acc.PubKey.(account.PubKeyNil); isNil {
			if _, isNil := in.PubKey.(account.PubKeyNil); isNil {
				return nil, block.ErrTxUnknownPubKey
			}
			if !bytes.Equal(in.PubKey.Address(), acc.Address) {
				return nil, block.ErrTxInvalidPubKey
			}
			acc.PubKey = in.PubKey
		} else {
			in.PubKey = account.PubKeyNil{}
		}
		accounts[string(in.Address)] = acc
	}
	for _, out := range outs {
		// Account shouldn't be duplicated
		if _, ok := accounts[string(out.Address)]; ok {
			return nil, block.ErrTxDuplicateAddress
		}
		acc := s.GetAccount(out.Address)
		// output account may be nil (new)
		if acc == nil {
			acc = &account.Account{
				Address:  out.Address,
				PubKey:   account.PubKeyNil{},
				Sequence: 0,
				Balance:  0,
			}
		}
		accounts[string(out.Address)] = acc
	}
	return accounts, nil
}

func (s *State) ValidateInputs(accounts map[string]*account.Account, signBytes []byte, ins []*block.TxInput) (total uint64, err error) {
	for _, in := range ins {
		acc := accounts[string(in.Address)]
		if acc == nil {
			panic("ValidateInputs() expects account in accounts")
		}
		// Check TxInput basic
		if err := in.ValidateBasic(); err != nil {
			return 0, err
		}
		// Check signatures
		if !acc.PubKey.VerifyBytes(signBytes, in.Signature) {
			return 0, block.ErrTxInvalidSignature
		}
		// Check sequences
		if acc.Sequence+1 != in.Sequence {
			return 0, block.ErrTxInvalidSequence
		}
		// Check amount
		if acc.Balance < in.Amount {
			return 0, block.ErrTxInsufficientFunds
		}
		// Good. Add amount to total
		total += in.Amount
	}
	return total, nil
}

func (s *State) ValidateOutputs(outs []*block.TxOutput) (total uint64, err error) {
	for _, out := range outs {
		// Check TxOutput basic
		if err := out.ValidateBasic(); err != nil {
			return 0, err
		}
		// Good. Add amount to total
		total += out.Amount
	}
	return total, nil
}

func (s *State) AdjustByInputs(accounts map[string]*account.Account, ins []*block.TxInput) {
	for _, in := range ins {
		acc := accounts[string(in.Address)]
		if acc == nil {
			panic("AdjustByInputs() expects account in accounts")
		}
		if acc.Balance < in.Amount {
			panic("AdjustByInputs() expects sufficient funds")
		}
		acc.Balance -= in.Amount
		acc.Sequence += 1
	}
}

func (s *State) AdjustByOutputs(accounts map[string]*account.Account, outs []*block.TxOutput) {
	for _, out := range outs {
		acc := accounts[string(out.Address)]
		if acc == nil {
			panic("AdjustByOutputs() expects account in accounts")
		}
		acc.Balance += out.Amount
	}
}

// If the tx is invalid, an error will be returned.
// Unlike AppendBlock(), state will not be altered.
func (s *State) ExecTx(tx_ block.Tx) error {

	// TODO: do something with fees
	fees := uint64(0)

	// Exec tx
	switch tx_.(type) {
	case *block.SendTx:
		tx := tx_.(*block.SendTx)
		accounts, err := s.GetOrMakeAccounts(tx.Inputs, tx.Outputs)
		if err != nil {
			return err
		}
		signBytes := account.SignBytes(tx)
		inTotal, err := s.ValidateInputs(accounts, signBytes, tx.Inputs)
		if err != nil {
			return err
		}
		outTotal, err := s.ValidateOutputs(tx.Outputs)
		if err != nil {
			return err
		}
		if outTotal > inTotal {
			return block.ErrTxInsufficientFunds
		}
		fee := inTotal - outTotal
		fees += fee

		// Good! Adjust accounts
		s.AdjustByInputs(accounts, tx.Inputs)
		s.AdjustByOutputs(accounts, tx.Outputs)
		s.UpdateAccounts(accounts)
		return nil

	case *block.BondTx:
		tx := tx_.(*block.BondTx)
		valInfo := s.GetValidatorInfo(tx.PubKey.Address())
		if valInfo != nil {
			// TODO: In the future, check that the validator wasn't destroyed,
			// add funds, merge UnbondTo outputs, and unbond validator.
			return errors.New("Adding coins to existing validators not yet supported")
		}
		accounts, err := s.GetOrMakeAccounts(tx.Inputs, nil)
		if err != nil {
			return err
		}
		signBytes := account.SignBytes(tx)
		inTotal, err := s.ValidateInputs(accounts, signBytes, tx.Inputs)
		if err != nil {
			return err
		}
		if err := tx.PubKey.ValidateBasic(); err != nil {
			return err
		}
		outTotal, err := s.ValidateOutputs(tx.UnbondTo)
		if err != nil {
			return err
		}
		if outTotal > inTotal {
			return block.ErrTxInsufficientFunds
		}
		fee := inTotal - outTotal
		fees += fee

		// Good! Adjust accounts
		s.AdjustByInputs(accounts, tx.Inputs)
		s.UpdateAccounts(accounts)
		// Add ValidatorInfo
		s.SetValidatorInfo(&ValidatorInfo{
			Address:         tx.PubKey.Address(),
			PubKey:          tx.PubKey,
			UnbondTo:        tx.UnbondTo,
			FirstBondHeight: s.LastBlockHeight + 1,
			FirstBondAmount: outTotal,
		})
		// Add Validator
		added := s.BondedValidators.Add(&Validator{
			Address:     tx.PubKey.Address(),
			PubKey:      tx.PubKey,
			BondHeight:  s.LastBlockHeight + 1,
			VotingPower: outTotal,
			Accum:       0,
		})
		if !added {
			panic("Failed to add validator")
		}
		return nil

	case *block.UnbondTx:
		tx := tx_.(*block.UnbondTx)

		// The validator must be active
		_, val := s.BondedValidators.GetByAddress(tx.Address)
		if val == nil {
			return block.ErrTxInvalidAddress
		}

		// Verify the signature
		signBytes := account.SignBytes(tx)
		if !val.PubKey.VerifyBytes(signBytes, tx.Signature) {
			return block.ErrTxInvalidSignature
		}

		// tx.Height must be greater than val.LastCommitHeight
		if tx.Height <= val.LastCommitHeight {
			return errors.New("Invalid unbond height")
		}

		// Good!
		s.unbondValidator(val)
		return nil

	case *block.RebondTx:
		tx := tx_.(*block.RebondTx)

		// The validator must be inactive
		_, val := s.UnbondingValidators.GetByAddress(tx.Address)
		if val == nil {
			return block.ErrTxInvalidAddress
		}

		// Verify the signature
		signBytes := account.SignBytes(tx)
		if !val.PubKey.VerifyBytes(signBytes, tx.Signature) {
			return block.ErrTxInvalidSignature
		}

		// tx.Height must be equal to the next height
		if tx.Height != s.LastBlockHeight+1 {
			return errors.New("Invalid rebond height")
		}

		// Good!
		s.rebondValidator(val)
		return nil

	case *block.DupeoutTx:
		tx := tx_.(*block.DupeoutTx)

		// Verify the signatures
		_, accused := s.BondedValidators.GetByAddress(tx.Address)
		voteASignBytes := account.SignBytes(&tx.VoteA)
		voteBSignBytes := account.SignBytes(&tx.VoteB)
		if !accused.PubKey.VerifyBytes(voteASignBytes, tx.VoteA.Signature) ||
			!accused.PubKey.VerifyBytes(voteBSignBytes, tx.VoteB.Signature) {
			return block.ErrTxInvalidSignature
		}

		// Verify equivocation
		// TODO: in the future, just require one vote from a previous height that
		// doesn't exist on this chain.
		if tx.VoteA.Height != tx.VoteB.Height {
			return errors.New("DupeoutTx heights don't match")
		}
		if tx.VoteA.Type == block.VoteTypeCommit && tx.VoteA.Round < tx.VoteB.Round {
			// Check special case.
			// Validators should not sign another vote after committing.
		} else {
			if tx.VoteA.Round != tx.VoteB.Round {
				return errors.New("DupeoutTx rounds don't match")
			}
			if tx.VoteA.Type != tx.VoteB.Type {
				return errors.New("DupeoutTx types don't match")
			}
			if bytes.Equal(tx.VoteA.BlockHash, tx.VoteB.BlockHash) {
				return errors.New("DupeoutTx blockhashes shouldn't match")
			}
		}

		// Good! (Bad validator!)
		s.destroyValidator(accused)
		return nil

	default:
		panic("Unknown Tx type")
	}
}

func (s *State) unbondValidator(val *Validator) {
	// Move validator to UnbondingValidators
	val, removed := s.BondedValidators.Remove(val.Address)
	if !removed {
		panic("Couldn't remove validator for unbonding")
	}
	val.UnbondHeight = s.LastBlockHeight + 1
	added := s.UnbondingValidators.Add(val)
	if !added {
		panic("Couldn't add validator for unbonding")
	}
}

func (s *State) rebondValidator(val *Validator) {
	// Move validator to BondingValidators
	val, removed := s.UnbondingValidators.Remove(val.Address)
	if !removed {
		panic("Couldn't remove validator for rebonding")
	}
	val.BondHeight = s.LastBlockHeight + 1
	added := s.BondedValidators.Add(val)
	if !added {
		panic("Couldn't add validator for rebonding")
	}
}

func (s *State) releaseValidator(val *Validator) {
	// Update validatorInfo
	valInfo := s.GetValidatorInfo(val.Address)
	if valInfo == nil {
		panic("Couldn't find validatorInfo for release")
	}
	valInfo.ReleasedHeight = s.LastBlockHeight + 1
	s.SetValidatorInfo(valInfo)

	// Send coins back to UnbondTo outputs
	accounts, err := s.GetOrMakeAccounts(nil, valInfo.UnbondTo)
	if err != nil {
		panic("Couldn't get or make unbondTo accounts")
	}
	s.AdjustByOutputs(accounts, valInfo.UnbondTo)
	s.UpdateAccounts(accounts)

	// Remove validator from UnbondingValidators
	_, removed := s.UnbondingValidators.Remove(val.Address)
	if !removed {
		panic("Couldn't remove validator for release")
	}
}

func (s *State) destroyValidator(val *Validator) {
	// Update validatorInfo
	valInfo := s.GetValidatorInfo(val.Address)
	if valInfo == nil {
		panic("Couldn't find validatorInfo for release")
	}
	valInfo.DestroyedHeight = s.LastBlockHeight + 1
	valInfo.DestroyedAmount = val.VotingPower
	s.SetValidatorInfo(valInfo)

	// Remove validator
	_, removed := s.BondedValidators.Remove(val.Address)
	if !removed {
		_, removed := s.UnbondingValidators.Remove(val.Address)
		if !removed {
			panic("Couldn't remove validator for destruction")
		}
	}

}

// "checkStateHash": If false, instead of checking the resulting
// state.Hash() against block.StateHash, it *sets* the block.StateHash.
// (used for constructing a new proposal)
// NOTE: If an error occurs during block execution, state will be left
// at an invalid state.  Copy the state before calling AppendBlock!
func (s *State) AppendBlock(block_ *block.Block, blockPartsHeader block.PartSetHeader, checkStateHash bool) error {
	// Basic block validation.
	err := block_.ValidateBasic(s.LastBlockHeight, s.LastBlockHash, s.LastBlockParts, s.LastBlockTime)
	if err != nil {
		return err
	}

	// Validate block Validation.
	if block_.Height == 1 {
		if len(block_.Validation.Commits) != 0 {
			return errors.New("Block at height 1 (first block) should have no Validation commits")
		}
	} else {
		if uint(len(block_.Validation.Commits)) != s.BondedValidators.Size() {
			return errors.New("Invalid block validation size")
		}
		var sumVotingPower uint64
		s.BondedValidators.Iterate(func(index uint, val *Validator) bool {
			commit := block_.Validation.Commits[index]
			if commit.IsZero() {
				return false
			} else {
				vote := &block.Vote{
					Height:     block_.Height - 1,
					Round:      commit.Round,
					Type:       block.VoteTypeCommit,
					BlockHash:  block_.LastBlockHash,
					BlockParts: block_.LastBlockParts,
				}
				if val.PubKey.VerifyBytes(account.SignBytes(vote), commit.Signature) {
					sumVotingPower += val.VotingPower
					return false
				} else {
					log.Warn(Fmt("Invalid validation signature.\nval: %v\nvote: %v", val, vote))
					err = errors.New("Invalid validation signature")
					return true
				}
			}
		})
		if err != nil {
			return err
		}
		if sumVotingPower <= s.BondedValidators.TotalVotingPower()*2/3 {
			return errors.New("Insufficient validation voting power")
		}
	}

	// Commit each tx
	for _, tx := range block_.Data.Txs {
		err := s.ExecTx(tx)
		if err != nil {
			return InvalidTxError{tx, err}
		}
	}

	// Update Validator.LastCommitHeight as necessary.
	for i, commit := range block_.Validation.Commits {
		if commit.IsZero() {
			continue
		}
		_, val := s.BondedValidators.GetByIndex(uint(i))
		if val == nil {
			panic(Fmt("Failed to fetch validator at index %v", i))
		}
		val.LastCommitHeight = block_.Height - 1
		updated := s.BondedValidators.Update(val)
		if !updated {
			panic("Failed to update validator LastCommitHeight")
		}
	}

	// If any unbonding periods are over,
	// reward account with bonded coins.
	toRelease := []*Validator{}
	s.UnbondingValidators.Iterate(func(index uint, val *Validator) bool {
		if val.UnbondHeight+unbondingPeriodBlocks < block_.Height {
			toRelease = append(toRelease, val)
		}
		return false
	})
	for _, val := range toRelease {
		s.releaseValidator(val)
	}

	// If any validators haven't signed in a while,
	// unbond them, they have timed out.
	toTimeout := []*Validator{}
	s.BondedValidators.Iterate(func(index uint, val *Validator) bool {
		if val.LastCommitHeight+validatorTimeoutBlocks < block_.Height {
			toTimeout = append(toTimeout, val)
		}
		return false
	})
	for _, val := range toTimeout {
		s.unbondValidator(val)
	}

	// Increment validator AccumPowers
	s.BondedValidators.IncrementAccum(1)

	// Check or set block.StateHash
	stateHash := s.Hash()
	if checkStateHash {
		// State hash should match
		if !bytes.Equal(stateHash, block_.StateHash) {
			return Errorf("Invalid state hash. Got %X, block says %X",
				stateHash, block_.StateHash)
		}
	} else {
		// Set the state hash.
		if block_.StateHash != nil {
			panic("Cannot overwrite block_.StateHash")
		}
		block_.StateHash = stateHash
	}

	s.LastBlockHeight = block_.Height
	s.LastBlockHash = block_.Hash()
	s.LastBlockParts = blockPartsHeader
	s.LastBlockTime = block_.Time
	return nil
}

// The returned Account is a copy, so mutating it
// has no side effects.
func (s *State) GetAccount(address []byte) *account.Account {
	_, acc := s.accounts.Get(address)
	if acc == nil {
		return nil
	}
	return acc.(*account.Account).Copy()
}

// The returned Account is a copy, so mutating it
// has no side effects.
func (s *State) GetAccounts() merkle.Tree {
	return s.accounts.Copy()
}

// The account is copied before setting, so mutating it
// afterwards has no side effects.
func (s *State) UpdateAccount(account *account.Account) {
	s.accounts.Set(account.Address, account.Copy())
}

// The accounts are copied before setting, so mutating it
// afterwards has no side effects.
func (s *State) UpdateAccounts(accounts map[string]*account.Account) {
	for _, acc := range accounts {
		s.accounts.Set(acc.Address, acc.Copy())
	}
}

// The returned ValidatorInfo is a copy, so mutating it
// has no side effects.
func (s *State) GetValidatorInfo(address []byte) *ValidatorInfo {
	_, valInfo := s.validatorInfos.Get(address)
	if valInfo == nil {
		return nil
	}
	return valInfo.(*ValidatorInfo).Copy()
}

// Returns false if new, true if updated.
// The valInfo is copied before setting, so mutating it
// afterwards has no side effects.
func (s *State) SetValidatorInfo(valInfo *ValidatorInfo) (updated bool) {
	return s.validatorInfos.Set(valInfo.Address, valInfo.Copy())
}

// Returns a hash that represents the state data,
// excluding LastBlock*
func (s *State) Hash() []byte {
	hashables := []merkle.Hashable{
		s.BondedValidators,
		s.UnbondingValidators,
		s.accounts,
		s.validatorInfos,
	}
	return merkle.HashFromHashables(hashables)
}
