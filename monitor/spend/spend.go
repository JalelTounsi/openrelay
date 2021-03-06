package spend

import (
	"encoding/json"
	"math/big"
	"context"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	// "github.com/ethereum/go-ethereum/core/types"
	"github.com/notegio/openrelay/funds"
	"github.com/notegio/openrelay/channels"
	"github.com/notegio/openrelay/db"
	"github.com/notegio/openrelay/types"
	"github.com/notegio/openrelay/exchangecontract"
	"github.com/notegio/openrelay/monitor/blocks"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"log"
	"strings"
	"fmt"
)

type spendBlockConsumer struct {
	tokenProxyAddress *types.Address
	spendTopic        *big.Int
	feeTokenAddress    string  // Needed for the SpendRecord,
	logFilter          ethereum.LogFilterer
	publisher          channels.Publisher
	balanceChecker     funds.BalanceChecker
}

func (consumer *spendBlockConsumer) Consume(delivery channels.Delivery) {
	block := &blocks.MiniBlock{}
	err := json.Unmarshal([]byte(delivery.Payload()), block)
	if err != nil {
		log.Printf("Error parsing payload: %v\n", err.Error())
	}
	if block.Bloom.Test(consumer.spendTopic) {
		log.Printf("Block %#x bloom filter indicates spend event", block.Hash)
		query := ethereum.FilterQuery{
			FromBlock: block.Number,
			ToBlock: block.Number,
			Addresses: nil,
			Topics: [][]common.Hash{
				[]common.Hash{common.BigToHash(consumer.spendTopic)},
				nil,
				nil,
			},
		}
		logs, err := consumer.logFilter.FilterLogs(context.Background(), query)
		if err != nil {
			delivery.Return()
			log.Fatalf("Failed to filter logs on block %v - aborting: %v", block.Number, err.Error())
		}
		log.Printf("Found %v spend logs", len(logs))
		tradedTokens := make(map[string]struct{})
		for _, spendLog := range logs {
			senderAddress := &types.Address{}
			tokenAddress := &types.Address{}
			copy(senderAddress[:], spendLog.Topics[2][12:])
			copy(tokenAddress[:], spendLog.Address[:])
			pairKey := fmt.Sprintf("%#x:%#x", senderAddress, tokenAddress)
			if _, ok := tradedTokens[pairKey]; ok {
				// If the same account sent the same token multiple times in a single
				// block, we already checked their balance as of the end of the block,
				// so we don't need to check it again.
				continue
			}
			tradedTokens[pairKey] = struct{}{}
			balance, err := consumer.balanceChecker.GetBalance(tokenAddress, senderAddress)
			if err != nil {
				if err.Error() == "abi: unmarshalling empty output" || err.Error() == "no contract code at given address" {
					balance = big.NewInt(0)
				} else {
					delivery.Return()
					log.Fatalf("Failed to get balance: %v", err.Error())
				}
			}
			allowance, err := consumer.balanceChecker.GetAllowance(tokenAddress, senderAddress, consumer.tokenProxyAddress)
			if err != nil {
				if err.Error() == "abi: unmarshalling empty output" || err.Error() == "no contract code at given address" {
					allowance = big.NewInt(0)
				} else {
					delivery.Return()
					log.Fatalf("Failed to get balance")
				}
			}
			if allowance.Cmp(balance) < 0 {
				// If allowance < balance, we should use that as our removal criteria
				balance = allowance
			}
			sr := &db.SpendRecord{
				TokenAddress: strings.ToLower(spendLog.Address.String()),
				SpenderAddress: hexutil.Encode(spendLog.Topics[1][12:]),
				ZrxToken: consumer.feeTokenAddress,
				Balance: balance.String(),
			}
			msg, err := json.Marshal(sr)
			if err != nil {
				delivery.Return()
				log.Fatalf("Failed to encode SpendRecord on block %v", block.Number)
			}
			consumer.publisher.Publish(string(msg))
		}
	} else {
		log.Printf("Block %v shows no spend events", block.Hash)
	}
	delivery.Ack()
}

func NewSpendBlockConsumer(tp *types.Address, feeToken string, lf ethereum.LogFilterer, publisher channels.Publisher, bc funds.BalanceChecker) (channels.Consumer) {
	spendTopic := &big.Int{}
	spendTopic.SetString("ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef", 16)
	return &spendBlockConsumer{tp, spendTopic, feeToken, lf, publisher, bc}
}

func NewRPCSpendBlockConsumer(rpcURL string, exchangeAddress string, publisher channels.Publisher) (channels.Consumer, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, err
	}
	exchange, err := exchangecontract.NewExchange(common.HexToAddress(exchangeAddress), client)
	if err != nil {
		log.Printf("Error intializing exchange contract '%v': '%v'", exchangeAddress, err.Error())
		return nil, err
	}
	feeTokenAddress, err := exchange.ZRX_TOKEN_CONTRACT(nil)
	if err != nil {
		log.Printf("error getting feeTokenAddress")
		return nil, err
	}
	tokenProxyAddress, err := exchange.TOKEN_TRANSFER_PROXY_CONTRACT(nil)
	if err != nil {
		log.Printf("error getting tokenProxyAddress")
		return nil, err
	}
	tokenProxyAddressOr := &types.Address{}
	copy(tokenProxyAddressOr[:], tokenProxyAddress[:])
	balanceChecker, err := funds.NewRpcBalanceChecker(rpcURL)
	if err != nil {
		log.Printf("Error getting balance checker")
		return nil, err
	}
	return NewSpendBlockConsumer(tokenProxyAddressOr, feeTokenAddress.Hex(), client, publisher, balanceChecker), nil
}
