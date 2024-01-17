package tapscript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/mssmt"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// ErrNoInputs represents an error case where an asset undergoing a
	// state transition does not have any or a specific input required.
	ErrNoInputs = errors.New("missing asset input(s)")

	// ErrInputMismatch represents an error case where an asset's set of
	// inputs mismatch the set provided to the virtual machine.
	ErrInputMismatch = errors.New("asset input(s) mismatch")

	// ErrInvalidScriptVersion represents an error case where an asset input
	// commits to an invalid script version.
	ErrInvalidScriptVersion = errors.New("invalid script version")
)

const (
	// zeroIndex is a constant that stores the usual zero index we use for
	// the virtual prev outs created in the VM.
	zeroIndex = 0
)

// virtualTxInputTree places all of the inputs of a virtual transaction that
// represents a taproot assets state transition into a MS-SMT. This tree is
// returned as the result.
func virtualTxInputTree(newAsset *asset.Asset, prevAssets commitment.InputSet) (
	mssmt.Tree, error) {

	inputTree := mssmt.NewCompactedTree(mssmt.NewDefaultStore())
	// For each input we'll locate the asset UTXO being spent, then
	// insert that into a new SMT, with the key being the hash of
	// the prevID pointer, and the value being the leaf itself.
	inputsConsumed := make(
		map[asset.PrevID]struct{}, len(prevAssets),
	)

	// TODO(bhandras): thread the context through.
	ctx := context.TODO()

	for _, input := range newAsset.PrevWitnesses {
		// At this point, each input MUST have a prev ID.
		if input.PrevID == nil {
			return nil, fmt.Errorf("%w: prevID is nil",
				ErrNoInputs)
		}

		// The set of prev assets are similar to the prev
		// output fetcher used in taproot.
		prevAsset, ok := prevAssets[*input.PrevID]
		if !ok {
			return nil, fmt.Errorf("%w: unable to make "+
				"virtual txIn %v", ErrNoInputs,
				spew.Sdump(input.PrevID))
		}

		// Now we'll insert this prev asset leaf into the tree.
		// The generated leaf includes the amount of the asset,
		// so the sum of this tree will be the total amount
		// being spent.
		key := input.PrevID.Hash()
		leaf, err := prevAsset.Leaf()
		if err != nil {
			return nil, err
		}
		_, err = inputTree.Insert(ctx, key, leaf)
		if err != nil {
			return nil, err
		}

		inputsConsumed[*input.PrevID] = struct{}{}
	}

	// In this context, the set of referenced inputs should match
	// the set of previous assets. This ensures no duplicate inputs
	// are being spent.
	//
	// TODO(roasbeef): make further explicit?
	if len(inputsConsumed) != len(prevAssets) {
		return nil, ErrInputMismatch
	}

	return inputTree, nil
}

// virtualTxIns computes the inputs of a Taproot Asset virtual transaction. Each
// input's prevout hash is the root of a MS-SMT containing the asset input at
// the same index.
func virtualTxIns(newAsset *asset.Asset, prevAssets commitment.InputSet) (
	[]*wire.TxIn, mssmt.Tree, error) {

	var (
		txIns    = make([]*wire.TxIn, len(newAsset.PrevWitnesses))
		aggrTree = mssmt.NewCompactedTree(mssmt.NewDefaultStore())
	)

	// For each input we'll locate the asset UTXO being spent, then
	// insert that into a new SMT, with the key being the hash of
	// the prevID pointer, and the value being the leaf itself.
	inputsConsumed := make(
		map[asset.PrevID]struct{}, len(prevAssets),
	)

	// TODO(bhandras): thread the context through.
	ctx := context.TODO()

	for _, input := range newAsset.PrevWitnesses {
		// At this point, each input MUST have a prev ID.
		if input.PrevID == nil {
			return nil, nil, fmt.Errorf("%w: prevID is nil",
				ErrNoInputs)
		}

		// The set of prev assets are similar to the prev
		// output fetcher used in taproot.
		prevAsset, ok := prevAssets[*input.PrevID]
		if !ok {
			return nil, nil, fmt.Errorf("%w: unable to make "+
				"virtual txIn %v", ErrNoInputs,
				spew.Sdump(input.PrevID))
		}

		// Now we'll insert this prev asset leaf into the tree.
		// The generated leaf includes the amount of the asset,
		// so the sum of this tree will be the total amount
		// being spent.
		key := input.PrevID.Hash()
		leaf, err := prevAsset.Leaf()
		if err != nil {
			return nil, nil, err
		}

		// Create the ms smt that will hold the input.
		inputTree := mssmt.NewCompactedTree(mssmt.NewDefaultStore())

		// Insert the leaf into the tree holding just this input.
		_, err = inputTree.Insert(ctx, key, leaf)
		if err != nil {
			return nil, nil, err
		}

		// Add the leaf to the aggregate input tree, which is going to
		// hold all the inputs.
		_, err = aggrTree.Insert(ctx, key, leaf)
		if err != nil {
			return nil, nil, err
		}

		inputsConsumed[*input.PrevID] = struct{}{}

		treeRoot, err := inputTree.Root(context.Background())
		if err != nil {
			return nil, nil, err
		}

		// TODO(roasbeef): document empty hash usage here
		prevOut := asset.VirtualTxInPrevOut(treeRoot)
		txIns = append(txIns, wire.NewTxIn(prevOut, nil, nil))
	}

	// In this context, the set of referenced inputs should match
	// the set of previous assets. This ensures no duplicate inputs
	// are being spent.
	//
	// TODO(roasbeef): make further explicit?
	if len(inputsConsumed) != len(prevAssets) {
		return nil, nil, ErrInputMismatch
	}

	return txIns, aggrTree, nil
}

// virtualTxOut computes the outputs of a Taproot Asset virtual transaction.
// Each output's pkScript commits to an MS-SMT containing the asset output at
// the same index.
func virtualTxOut(assetOutputs []*asset.Asset) ([]*wire.TxOut, error) {
	var (
		txOuts = make([]*wire.TxOut, 0, len(assetOutputs))
	)

	for _, txAsset := range assetOutputs {
		// If we have any asset splits, then we'll indirectly commit to
		// all of them through the SplitCommitmentRoot.
		if txAsset.SplitCommitmentRoot != nil {
			// In this case, we already have an MS-SMT over the set
			// of outputs created, so we'll map this into a normal
			// taproot (segwit v1) script.
			rootKey := txAsset.SplitCommitmentRoot.NodeHash()
			pkScript, err := asset.ComputeTaprootScript(rootKey[:])
			if err != nil {
				return nil, err
			}
			value := int64(txAsset.SplitCommitmentRoot.NodeSum())
			txOuts = append(txOuts, wire.NewTxOut(value, pkScript))
		}

		// Otherwise, we'll just commit to the new asset directly. In
		// this case, the output script is derived from the root of a
		// MS-SMT containing the new asset.
		var groupKey []byte
		if txAsset.GroupKey != nil {
			groupKey = schnorr.SerializePubKey(
				&txAsset.GroupKey.GroupPubKey,
			)
		} else {
			var emptyKey [32]byte
			groupKey = emptyKey[:]
		}
		assetID := txAsset.Genesis.ID()

		// TODO(roasbeef): double check this key matches the split
		// commitment above? or can treat as standalone case (no splits)
		h := sha256.New()
		_, _ = h.Write(groupKey)
		_, _ = h.Write(assetID[:])
		_, _ = h.Write(schnorr.SerializePubKey(txAsset.ScriptKey.PubKey))

		// The new asset may have witnesses for its input(s), so make a
		// copy and strip them out when including the asset in the tree,
		// as the witness depends on the result of the tree.
		//
		// TODO(roasbeef): ensure this is documented in the BIP
		copyWithoutWitness := txAsset.Copy()
		for i := range copyWithoutWitness.PrevWitnesses {
			copyWithoutWitness.PrevWitnesses[i].TxWitness = nil
		}
		key := *(*[32]byte)(h.Sum(nil))
		leaf, err := copyWithoutWitness.Leaf()
		if err != nil {
			return nil, err
		}
		outputTree := mssmt.NewCompactedTree(mssmt.NewDefaultStore())

		var (
			tree mssmt.Tree
		)

		// TODO(bhandras): thread the context through.
		tree, err = outputTree.Insert(context.TODO(), key, leaf)
		if err != nil {
			return nil, err
		}

		treeRoot, err := tree.Root(context.Background())
		if err != nil {
			return nil, err
		}

		rootKey := treeRoot.NodeHash()
		pkScript, err := asset.ComputeTaprootScript(rootKey[:])
		if err != nil {
			return nil, err
		}

		txOuts = append(
			txOuts, wire.NewTxOut(int64(txAsset.Amount), pkScript),
		)
	}

	return txOuts, nil
}

// VirtualTx constructs the virtual transaction that enables the movement of an
// asset representing an asset state transition. The inputs and outputs of the
// virtual transaction directly match the asset inputs and outputs of the
// transition.
func VirtualTx(newAsset *asset.Asset, prevAssets commitment.InputSet,
	assetOutputs []*asset.Asset) (*wire.MsgTx, mssmt.Tree, error) {

	var (
		txIns     = make([]*wire.TxIn, 0, len(newAsset.PrevWitnesses))
		inputTree mssmt.Tree
		err       error
	)

	// We'll start by creating the virtual transaction inputs. A tree
	// containing all the inputs is also returned, which may be used to
	// validate that assets are not being inflated.
	if newAsset.NeedsGenesisWitnessForGroup() ||
		newAsset.HasGenesisWitnessForGroup() {

		txIns, inputTree, err = asset.VirtualGenesisTxIn(newAsset)
	} else {
		txIns, inputTree, err = virtualTxIns(newAsset, prevAssets)
	}
	if err != nil {
		return nil, nil, err
	}

	// Then we'll map all asset outputs into a single UTXO.
	txOuts, err := virtualTxOut(assetOutputs)
	if err != nil {
		return nil, nil, err
	}

	// With our inputs and outputs, we're ready to construct our virtual
	// transaction.
	virtualTx := wire.NewMsgTx(2)

	// We add each input as a standard transaction input.
	for _, txIn := range txIns {
		virtualTx.AddTxIn(txIn)
	}

	// We add each output as a standard transaction output.
	for _, txOut := range txOuts {
		virtualTx.AddTxOut(txOut)
	}

	return virtualTx, inputTree, nil
}

// InputAssetPrevOut returns a TxOut that represents the input asset in a
// Taproot Asset virtual TX.
func InputAssetPrevOut(prevAsset asset.Asset) (*wire.TxOut, error) {
	switch prevAsset.ScriptVersion {
	case asset.ScriptV0:
		pkScript, err := PayToTaprootScript(prevAsset.ScriptKey.PubKey)
		if err != nil {
			return nil, err
		}

		return &wire.TxOut{
			Value:    int64(prevAsset.Amount),
			PkScript: pkScript,
		}, nil
	default:
		return nil, ErrInvalidScriptVersion
	}
}

// InputPrevOutFetcher returns a Taproot Asset input's `PrevOutFetcher` to be
// used throughout signing.
func InputPrevOutFetcher(prevAsset asset.Asset) (*txscript.CannedPrevOutputFetcher,
	error) {

	prevOut, err := InputAssetPrevOut(prevAsset)
	if err != nil {
		return nil, err
	}

	return txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	), nil
}

// InputKeySpendSigHash returns the signature hash of a virtual transaction for
// a specific Taproot Asset input that can be spent through the key path. This
// is the message over which signatures are generated over.
func InputKeySpendSigHash(virtualTx *wire.MsgTx, input *asset.Asset,
	idx uint32, sigHashType txscript.SigHashType) ([]byte, error) {

	virtualTxCopy := asset.VirtualTxWithInput(virtualTx, input, idx, nil)
	prevOutFetcher, err := InputPrevOutFetcher(*input)
	if err != nil {
		return nil, err
	}
	sigHashes := txscript.NewTxSigHashes(virtualTxCopy, prevOutFetcher)
	return txscript.CalcTaprootSignatureHash(
		sigHashes, sigHashType, virtualTxCopy, zeroIndex,
		prevOutFetcher,
	)
}

// InputScriptSpendSigHash returns the signature hash of a virtual transaction
// for a specific Taproot Asset input that can be spent through the script path.
// This is the message over which signatures are generated over.
func InputScriptSpendSigHash(virtualTx *wire.MsgTx, input *asset.Asset,
	idx uint32, sigHashType txscript.SigHashType,
	tapLeaf *txscript.TapLeaf) ([]byte, error) {

	virtualTxCopy := asset.VirtualTxWithInput(virtualTx, input, idx, nil)
	prevOutFetcher, err := InputPrevOutFetcher(*input)
	if err != nil {
		return nil, err
	}
	sigHashes := txscript.NewTxSigHashes(virtualTxCopy, prevOutFetcher)
	return txscript.CalcTapscriptSignaturehash(
		sigHashes, sigHashType, virtualTxCopy, zeroIndex,
		prevOutFetcher, *tapLeaf,
	)
}

// CreateTaprootSignature creates a Taproot signature for the given asset input.
// Depending on the fields set in the input, this will either create a key path
// spend or a script path spend.
func CreateTaprootSignature(vIn *tappsbt.VInput, virtualTx *wire.MsgTx,
	idx int, txSigner Signer) (wire.TxWitness, error) {

	// Before we even attempt to sign anything, we need to make sure all the
	// input information we require is present.
	if len(vIn.TaprootBip32Derivation) == 0 {
		return nil, fmt.Errorf("missing input Taproot BIP-0032 " +
			"derivation")
	}

	// Currently, we only support creating one signature per input.
	//
	// TODO(guggero): Should we support signing multiple paths at the same
	// time? What are the performance and security implications?
	if len(vIn.TaprootBip32Derivation) > 1 {
		return nil, fmt.Errorf("unsupported multiple taproot " +
			"BIP-0032 derivation info found, can only sign for " +
			"one at a time")
	}
	if len(vIn.TaprootBip32Derivation[0].LeafHashes) > 1 {
		return nil, fmt.Errorf("unsupported number of leaf hashes in " +
			"taproot BIP-0032 derivation info, can only sign for " +
			"one at a time")
	}

	derivation := vIn.TaprootBip32Derivation[0]

	// Compute a virtual prevOut from the input asset for the signer.
	prevOut, err := InputAssetPrevOut(*vIn.Asset())
	if err != nil {
		return nil, err
	}

	// Start with a default sign descriptor and the BIP-0086 sign method
	// then adjust depending on the input parameters.
	spendDesc := lndclient.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: vIn.Asset().ScriptKey.RawKey.PubKey,
		},
		SignMethod: input.TaprootKeySpendBIP0086SignMethod,
		Output:     prevOut,
		HashType:   vIn.SighashType,
		InputIndex: idx,
	}

	// There are three possible signing cases: BIP-0086 key spend path, key
	// spend path with a script root, and script spend path.
	switch {
	// If there is no merkle root, we're doing a BIP-0086 key spend.
	case len(vIn.TaprootMerkleRoot) == 0:
		// This is the default case, so we don't need to do anything.

	// No leaf hash means we're not signing a specific script, so this is
	// the key spend path with a script root.
	case len(vIn.TaprootMerkleRoot) == sha256.Size &&
		len(derivation.LeafHashes) == 0:

		spendDesc.SignMethod = input.TaprootKeySpendSignMethod
		spendDesc.TapTweak = vIn.TaprootMerkleRoot

	// One leaf hash and a merkle root means we're signing a specific
	// script. There can be other scripts in the tree, but we only support
	// creating a signature for a single one at a time.
	case len(vIn.TaprootMerkleRoot) == sha256.Size &&
		len(derivation.LeafHashes) == 1:

		// If we're supposed to be signing for a leaf hash, we also
		// expect the leaf script that hashes to that hash in the
		// appropriate field.
		if len(vIn.TaprootLeafScript) != 1 {
			return nil, fmt.Errorf("specified leaf hash in " +
				"taproot BIP-0032 derivation but missing " +
				"taproot leaf script")
		}

		leafScript := vIn.TaprootLeafScript[0]
		leaf := txscript.TapLeaf{
			LeafVersion: leafScript.LeafVersion,
			Script:      leafScript.Script,
		}
		leafHash := leaf.TapHash()
		if !bytes.Equal(leafHash[:], derivation.LeafHashes[0]) {
			return nil, fmt.Errorf("specified leaf hash in " +
				"taproot BIP-0032 derivation but " +
				"corresponding taproot leaf script was not " +
				"found")
		}

		spendDesc.SignMethod = input.TaprootScriptSpendSignMethod
		spendDesc.WitnessScript = leafScript.Script

	// Some invalid combination of fields was specified, it's not clear what
	// we should do. So rather than fail later, let's return an explicit
	// error here.
	default:
		return nil, fmt.Errorf("unable to determine signing method " +
			"from virtual transaction packet")
	}

	sig, err := txSigner.SignVirtualTx(&spendDesc, virtualTx, prevOut)
	if err != nil {
		return nil, err
	}

	witness := wire.TxWitness{sig.Serialize()}
	if vIn.SighashType != txscript.SigHashDefault {
		witness[0] = append(witness[0], byte(vIn.SighashType))
	}

	// If this was a script spend, we also have to add the script itself and
	// the control block to the witness, otherwise the verifier will reject
	// the generated witness.
	if spendDesc.SignMethod == input.TaprootScriptSpendSignMethod {
		witness = append(witness, spendDesc.WitnessScript)
		witness = append(witness, vIn.TaprootLeafScript[0].ControlBlock)
	}

	return witness, nil
}
