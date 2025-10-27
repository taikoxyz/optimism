package p2p

import (
    "bytes"
    "math/big"
    "testing"

    "github.com/ethereum/go-ethereum/common"
    "github.com/stretchr/testify/require"
)

func TestPreconfirmation_MarshalUnmarshal_RoundTrip(t *testing.T) {
    // Construct a Preconfirmation with diverse values
    max255 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1))
    p := Preconfirmation{
        EOP:                      true,
        BlockNumber:              big.NewInt(1234567890123456789),
        AnchorBlockNumber:        big.NewInt(0),
        ParentRawTxListHash:      common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
        RawTxListHash:            common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222"),
        ParentSubmissionWindowEnd: max255,
        SubmissionWindowEnd:      big.NewInt(42),
    }

    var buf bytes.Buffer
    n, err := p.MarshalSSZ(&buf)
    require.NoError(t, err)
    require.Equal(t, preconfSSZSize, n)
    require.Len(t, buf.Bytes(), preconfSSZSize)

    var q Preconfirmation
    err = q.UnmarshalSSZ(uint32(preconfSSZSize), bytes.NewReader(buf.Bytes()))
    require.NoError(t, err)

    require.Equal(t, p.EOP, q.EOP)
    require.Equal(t, 0, p.BlockNumber.Cmp(q.BlockNumber))
    require.Equal(t, 0, p.AnchorBlockNumber.Cmp(q.AnchorBlockNumber))
    require.Equal(t, p.ParentRawTxListHash, q.ParentRawTxListHash)
    require.Equal(t, p.RawTxListHash, q.RawTxListHash)
    require.Equal(t, 0, p.ParentSubmissionWindowEnd.Cmp(q.ParentSubmissionWindowEnd))
    require.Equal(t, 0, p.SubmissionWindowEnd.Cmp(q.SubmissionWindowEnd))
}

func TestPreconfirmation_MarshalSSZ_NilBigIntErrors(t *testing.T) {
    base := Preconfirmation{
        EOP:                      false,
        BlockNumber:              big.NewInt(1),
        AnchorBlockNumber:        big.NewInt(2),
        ParentRawTxListHash:      common.Hash{},
        RawTxListHash:            common.Hash{},
        ParentSubmissionWindowEnd: big.NewInt(3),
        SubmissionWindowEnd:      big.NewInt(4),
    }

    tests := []struct {
        name string
        mut  func(p *Preconfirmation)
    }{
        {"NilBlockNumber", func(p *Preconfirmation) { p.BlockNumber = nil }},
        {"NilAnchorBlockNumber", func(p *Preconfirmation) { p.AnchorBlockNumber = nil }},
        {"NilParentSubmissionWindowEnd", func(p *Preconfirmation) { p.ParentSubmissionWindowEnd = nil }},
        {"NilSubmissionWindowEnd", func(p *Preconfirmation) { p.SubmissionWindowEnd = nil }},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            p := base
            tt.mut(&p)
            var buf bytes.Buffer
            _, err := p.MarshalSSZ(&buf)
            require.Error(t, err)
        })
    }
}

func TestPreconfirmation_MarshalSSZ_LayoutLittleEndian(t *testing.T) {
    // Choose small values to make byte order checks easy
    p := Preconfirmation{
        EOP:                      true,
        BlockNumber:              big.NewInt(0x0102), // 258 -> 0x02, 0x01 in LE
        AnchorBlockNumber:        big.NewInt(0x0A0B0C),
        ParentRawTxListHash:      common.Hash{},
        RawTxListHash:            common.Hash{},
        ParentSubmissionWindowEnd: big.NewInt(0),
        SubmissionWindowEnd:      big.NewInt(0),
    }

    var buf bytes.Buffer
    _, err := p.MarshalSSZ(&buf)
    require.NoError(t, err)
    data := buf.Bytes()
    require.Len(t, data, preconfSSZSize)

    // Byte 0: EOP
    require.Equal(t, byte(1), data[0])

    // Bytes 1..32: BlockNumber little-endian, expect 0x02, 0x01, then zeros
    require.Equal(t, byte(0x02), data[1])
    require.Equal(t, byte(0x01), data[2])
    for i := 3; i < 33; i++ {
        require.Equal(t, byte(0x00), data[i])
    }
}

func TestPreconfirmation_UnmarshalSSZ_BadScope(t *testing.T) {
    p := Preconfirmation{
        EOP:                      false,
        BlockNumber:              big.NewInt(1),
        AnchorBlockNumber:        big.NewInt(2),
        ParentRawTxListHash:      common.Hash{},
        RawTxListHash:            common.Hash{},
        ParentSubmissionWindowEnd: big.NewInt(3),
        SubmissionWindowEnd:      big.NewInt(4),
    }

    var buf bytes.Buffer
    _, err := p.MarshalSSZ(&buf)
    require.NoError(t, err)

    var out Preconfirmation
    // Intentionally wrong scope
    err = out.UnmarshalSSZ(uint32(preconfSSZSize+1), bytes.NewReader(buf.Bytes()))
    require.Error(t, err)
}

