package p2p

import (
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Preconfirmation represents the object that is preconfirmed over the network.
type Preconfirmation struct {
	EOP bool

	BlockNumber               *big.Int
	AnchorBlockNumber         *big.Int
	ParentRawTxListHash       common.Hash
	RawTxListHash             common.Hash
	ParentSubmissionWindowEnd *big.Int
	SubmissionWindowEnd       *big.Int
}

// PreconfCommitment binds a preconfirmation with the slasher address committing to it.
type PreconfCommitment struct {
	Preconf        Preconfirmation
	SlasherAddress common.Address
}

// SignedCommitment is gossiped post-whitelist; it carries a commitment and its signature.
type SignedCommitment struct {
	Commitment PreconfCommitment
	Signature  []byte
}

const (
	preconfSSZSize      = 1 + 32 + 32 + 32 + 32 + 32 + 32 // bool + 4x uint256 + 2x hash
	commitmentSSZSize   = preconfSSZSize + 20             // + address
	signedCommitSSZSize = commitmentSSZSize + 65          // + signature
)

func putLE32FromBig(out []byte, v *big.Int) error {
	if v == nil {
		return errors.New("nil big.Int")
	}
	b := v.Bytes() // big-endian
	if len(b) > 32 {
		return fmt.Errorf("big.Int too large: %d bytes", len(b))
	}
	// Write little-endian into out
	for i := 0; i < len(b); i++ {
		out[i] = b[len(b)-1-i]
	}
	for i := len(b); i < 32; i++ {
		out[i] = 0
	}
	return nil
}

func getBigFromLE32(in []byte) *big.Int {
	// reverse to big-endian
	be := make([]byte, 32)
	for i := 0; i < 32; i++ {
		be[31-i] = in[i]
	}
	return new(big.Int).SetBytes(be)
}

func (p *Preconfirmation) MarshalSSZ(w io.Writer) (int, error) {
	buf := make([]byte, preconfSSZSize)
	off := 0
	if p.EOP {
		buf[off] = 1
	} else {
		buf[off] = 0
	}
	off += 1

	if err := putLE32FromBig(buf[off:off+32], p.BlockNumber); err != nil {
		return 0, err
	}
	off += 32
	if err := putLE32FromBig(buf[off:off+32], p.AnchorBlockNumber); err != nil {
		return 0, err
	}
	off += 32
	copy(buf[off:off+32], p.ParentRawTxListHash[:])
	off += 32
	copy(buf[off:off+32], p.RawTxListHash[:])
	off += 32
	if err := putLE32FromBig(buf[off:off+32], p.ParentSubmissionWindowEnd); err != nil {
		return 0, err
	}
	off += 32
	if err := putLE32FromBig(buf[off:off+32], p.SubmissionWindowEnd); err != nil {
		return 0, err
	}
	off += 32
	if off != preconfSSZSize {
		return 0, fmt.Errorf("internal preconf size mismatch: %d", off)
	}
	return w.Write(buf)
}

func (p *Preconfirmation) UnmarshalSSZ(scope uint32, r io.Reader) error {
	if scope != preconfSSZSize {
		return fmt.Errorf("unexpected preconf scope: %d", scope)
	}
	buf := make([]byte, preconfSSZSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	off := 0
	p.EOP = buf[off] == 1
	off += 1
	p.BlockNumber = getBigFromLE32(buf[off : off+32])
	off += 32
	p.AnchorBlockNumber = getBigFromLE32(buf[off : off+32])
	off += 32
	copy(p.ParentRawTxListHash[:], buf[off:off+32])
	off += 32
	copy(p.RawTxListHash[:], buf[off:off+32])
	off += 32
	p.ParentSubmissionWindowEnd = getBigFromLE32(buf[off : off+32])
	off += 32
	p.SubmissionWindowEnd = getBigFromLE32(buf[off : off+32])
	off += 32
	if off != preconfSSZSize {
		return fmt.Errorf("internal preconf size mismatch: %d", off)
	}
	return nil
}

func (c *PreconfCommitment) MarshalSSZ(w io.Writer) (int, error) {
	// preconf + slasher
	buf := make([]byte, commitmentSSZSize)
	// write preconf at start using MarshalSSZ into buffer
	n, err := c.Preconf.MarshalSSZ(sliceWriter(buf[0:preconfSSZSize]))
	if err != nil {
		return 0, err
	}
	if n != preconfSSZSize {
		return 0, fmt.Errorf("unexpected preconf marshal size: %d", n)
	}
	copy(buf[preconfSSZSize:preconfSSZSize+20], c.SlasherAddress[:])
	return w.Write(buf)
}

func (c *PreconfCommitment) UnmarshalSSZ(scope uint32, r io.Reader) error {
	if scope != commitmentSSZSize {
		return fmt.Errorf("unexpected commitment scope: %d", scope)
	}
	buf := make([]byte, commitmentSSZSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if err := c.Preconf.UnmarshalSSZ(preconfSSZSize, sliceReader(buf[0:preconfSSZSize])); err != nil {
		return err
	}
	copy(c.SlasherAddress[:], buf[preconfSSZSize:preconfSSZSize+20])
	return nil
}

func (sc *SignedCommitment) MarshalSSZ(w io.Writer) (int, error) {
	if len(sc.Signature) != 65 {
		return 0, fmt.Errorf("signature must be 65 bytes, got %d", len(sc.Signature))
	}
	buf := make([]byte, signedCommitSSZSize)
	n, err := sc.Commitment.MarshalSSZ(sliceWriter(buf[0:commitmentSSZSize]))
	if err != nil {
		return 0, err
	}
	if n != commitmentSSZSize {
		return 0, fmt.Errorf("unexpected commitment marshal size: %d", n)
	}
	copy(buf[commitmentSSZSize:], sc.Signature[:])
	return w.Write(buf)
}

func (sc *SignedCommitment) UnmarshalSSZ(scope uint32, r io.Reader) error {
	if scope != signedCommitSSZSize {
		return fmt.Errorf("unexpected signed commitment scope: %d", scope)
	}
	buf := make([]byte, signedCommitSSZSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if err := sc.Commitment.UnmarshalSSZ(commitmentSSZSize, sliceReader(buf[0:commitmentSSZSize])); err != nil {
		return err
	}
	sig := make([]byte, 65)
	copy(sig, buf[commitmentSSZSize:])
	sc.Signature = sig
	return nil
}

// Helpers to use byte slices as io.Writer/io.Reader without allocations.
type sliceWriter []byte

func (w sliceWriter) Write(p []byte) (int, error) {
	if len(p) != len(w) {
		return 0, fmt.Errorf("sliceWriter: size mismatch %d != %d", len(p), len(w))
	}
	copy(w, p)
	return len(p), nil
}

type sliceReader []byte

func (r sliceReader) Read(p []byte) (int, error) {
	if len(p) != len(r) {
		return 0, fmt.Errorf("sliceReader: size mismatch %d != %d", len(p), len(r))
	}
	copy(p, r)
	return len(p), nil
}
