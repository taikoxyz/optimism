package txmgr

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// EthClient is a wrapper for go-ethereum eth client
type EthClient struct {
	gethClient *gethclient.Client
	ethClient  *ethclient.Client
}

func NewEthClient(ctx context.Context, url string) (*EthClient, error) {
	client, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}

	ethClient := ethclient.NewClient(client)

	return &EthClient{
		gethClient: gethclient.New(client),
		ethClient:  ethClient,
	}, nil
}

func (c *EthClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	return c.ethClient.TransactionReceipt(ctx, txHash)
}

func (c *EthClient) Close() {
	c.ethClient.Close()
}

func (c *EthClient) ChainID(ctx context.Context) (*big.Int, error) {
	return c.ethClient.ChainID(ctx)
}

func (c *EthClient) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	return c.ethClient.BlockByNumber(ctx, number)
}

func (c *EthClient) BlockNumber(ctx context.Context) (uint64, error) {
	return c.ethClient.BlockNumber(ctx)
}

func (c *EthClient) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return c.ethClient.HeaderByHash(ctx, hash)
}

func (c *EthClient) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return c.ethClient.HeaderByNumber(ctx, number)
}

func (c *EthClient) SyncProgress(ctx context.Context) (*ethereum.SyncProgress, error) {
	return c.ethClient.SyncProgress(ctx)
}

func (c *EthClient) NetworkID(ctx context.Context) (*big.Int, error) {
	return c.ethClient.NetworkID(ctx)
}

func (c *EthClient) BalanceAt(
	ctx context.Context,
	account common.Address,
	blockNumber *big.Int,
) (*big.Int, error) {
	return c.ethClient.BalanceAt(ctx, account, blockNumber)
}

func (c *EthClient) StorageAt(
	ctx context.Context,
	account common.Address,
	key common.Hash,
	blockNumber *big.Int,
) ([]byte, error) {
	return c.ethClient.StorageAt(ctx, account, key, blockNumber)
}

func (c *EthClient) CodeAt(
	ctx context.Context,
	account common.Address,
	blockNumber *big.Int,
) ([]byte, error) {
	return c.ethClient.CodeAt(ctx, account, blockNumber)
}

func (c *EthClient) NonceAt(
	ctx context.Context,
	account common.Address,
	blockNumber *big.Int,
) (uint64, error) {
	return c.ethClient.NonceAt(ctx, account, blockNumber)
}

func (c *EthClient) PendingBalanceAt(ctx context.Context, account common.Address) (*big.Int, error) {
	return c.ethClient.PendingBalanceAt(ctx, account)
}

func (c *EthClient) PendingStorageAt(
	ctx context.Context,
	account common.Address,
	key common.Hash,
) ([]byte, error) {
	return c.ethClient.PendingStorageAt(ctx, account, key)
}

func (c *EthClient) PendingCodeAt(ctx context.Context, account common.Address) ([]byte, error) {
	return c.ethClient.PendingCodeAt(ctx, account)
}

func (c *EthClient) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return c.ethClient.PendingNonceAt(ctx, account)
}

func (c *EthClient) PendingTransactionCount(ctx context.Context) (uint, error) {
	return c.ethClient.PendingTransactionCount(ctx)
}

func (c *EthClient) CallContract(
	ctx context.Context,
	msg ethereum.CallMsg,
	blockNumber *big.Int,
) ([]byte, error) {
	return c.ethClient.CallContract(ctx, msg, blockNumber)
}

func (c *EthClient) CallContractAtHash(
	ctx context.Context,
	msg ethereum.CallMsg,
	blockHash common.Hash,
) ([]byte, error) {
	return c.ethClient.CallContractAtHash(ctx, msg, blockHash)
}

func (c *EthClient) PendingCallContract(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	return c.ethClient.PendingCallContract(ctx, msg)
}

func (c *EthClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return c.ethClient.SuggestGasPrice(ctx)
}

func (c *EthClient) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return c.ethClient.SuggestGasTipCap(ctx)
}

func (c *EthClient) FeeHistory(
	ctx context.Context,
	blockCount uint64,
	lastBlock *big.Int,
	rewardPercentiles []float64,
) (*ethereum.FeeHistory, error) {
	return c.ethClient.FeeHistory(ctx, blockCount, lastBlock, rewardPercentiles)
}

func (c *EthClient) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	return c.ethClient.EstimateGas(ctx, msg)
}

func (c *EthClient) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	return c.ethClient.SendTransaction(ctx, tx)
}

func (c *EthClient) CreateAccessList(ctx context.Context, msg ethereum.CallMsg) (*types.AccessList, uint64, string, error) {
	return c.gethClient.CreateAccessList(ctx, msg)
}
