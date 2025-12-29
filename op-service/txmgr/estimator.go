package txmgr

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

type GasPriceEstimatorFn func(ctx context.Context, backend ETHBackend) (*big.Int, *big.Int, *big.Int, error)

func DefaultGasPriceEstimatorFn(ctx context.Context, backend ETHBackend) (*big.Int, *big.Int, *big.Int, error) {
	tip, err := backend.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	head, err := backend.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if head.BaseFee == nil {
		return nil, nil, nil, errors.New("txmgr does not support pre-london blocks that do not have a base fee")
	}

	var blobFee *big.Int
	if head.ExcessBlobGas != nil {
		var err error
		blobFee, err = fetchBlobBaseFee(ctx, backend)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return tip, head.BaseFee, blobFee, nil
}

type blobBaseFeeBackend interface {
	BlobBaseFee(ctx context.Context) (*big.Int, error)
}

type rpcBackend interface {
	Client() *rpc.Client
}

func fetchBlobBaseFee(ctx context.Context, backend ETHBackend) (*big.Int, error) {
	if b, ok := backend.(blobBaseFeeBackend); ok {
		return b.BlobBaseFee(ctx)
	}
	if b, ok := backend.(rpcBackend); ok {
		var result hexutil.Big
		if err := b.Client().CallContext(ctx, &result, "eth_blobBaseFee"); err != nil {
			return nil, err
		}
		return (*big.Int)(&result), nil
	}
	return nil, errors.New("backend does not support blob base fee rpc")
}
