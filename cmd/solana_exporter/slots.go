package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/asymmetric-research/solana_exporter/pkg/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
)

const (
	slotPacerSchedule = 1 * time.Second
)

type SlotWatcher struct {
	client rpc.Provider

	// config:
	leaderSlotAddresses      []string
	inflationRewardAddresses []string
	feeRewardAddresses       []string

	// currentEpoch is the current epoch we are watching
	currentEpoch int64
	// firstSlot is the first slot [inclusive] of the current epoch which we are watching
	firstSlot int64
	// lastSlot is the last slot [inclusive] of the current epoch which we are watching
	lastSlot int64
	// slotWatermark is the last (most recent) slot we have tracked
	slotWatermark int64

	leaderSchedule map[string][]int64
}

var (
	totalTransactionsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "solana_confirmed_transactions_total",
		Help: "Total number of transactions processed since genesis (max confirmation)",
	})

	confirmedSlotHeight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "solana_confirmed_slot_height",
		Help: "Last confirmed slot height processed by watcher routine (max confirmation)",
	})

	currentEpochNumber = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "solana_confirmed_epoch_number",
		Help: "Current epoch (max confirmation)",
	})

	epochFirstSlot = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "solana_confirmed_epoch_first_slot",
		Help: "Current epoch's first slot (max confirmation)",
	})

	epochLastSlot = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "solana_confirmed_epoch_last_slot",
		Help: "Current epoch's last slot (max confirmation)",
	})

	leaderSlotsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "solana_leader_slots_total",
			Help: "(DEPRECATED) Number of leader slots per leader, grouped by skip status",
		},
		[]string{"status", "nodekey"},
	)

	leaderSlotsByEpoch = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "solana_leader_slots_by_epoch",
			Help: "Number of leader slots per leader, grouped by skip status and epoch",
		},
		[]string{"status", "nodekey", "epoch"},
	)

	inflationRewards = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "solana_inflation_rewards",
			Help: "Inflation reward earned per validator vote account, per epoch",
		},
		[]string{"votekey", "epoch"},
	)

	feeRewards = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "solana_fee_rewards",
			Help: "Transaction fee rewards earned per validator identity account, per epoch",
		},
		[]string{"nodekey", "epoch"},
	)
)

func NewCollectorSlotWatcher(collector *solanaCollector) *SlotWatcher {
	return &SlotWatcher{
		client:                   collector.rpcClient,
		leaderSlotAddresses:      collector.leaderSlotAddresses,
		inflationRewardAddresses: collector.inflationRewardAddresses,
		feeRewardAddresses:       collector.feeRewardAddresses,
	}
}

func init() {
	prometheus.MustRegister(totalTransactionsTotal)
	prometheus.MustRegister(confirmedSlotHeight)
	prometheus.MustRegister(currentEpochNumber)
	prometheus.MustRegister(epochFirstSlot)
	prometheus.MustRegister(epochLastSlot)
	prometheus.MustRegister(leaderSlotsTotal)
	prometheus.MustRegister(leaderSlotsByEpoch)
	prometheus.MustRegister(inflationRewards)
	prometheus.MustRegister(feeRewards)
}

func (c *SlotWatcher) WatchSlots(ctx context.Context, pace time.Duration) {
	ticker := time.NewTicker(pace)
	defer ticker.Stop()

	klog.Infof("Starting slot watcher")

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Stopping WatchSlots() at slot %v", c.slotWatermark)
			return
		default:
			<-ticker.C

			ctx_, cancel := context.WithTimeout(ctx, httpTimeout)
			// TODO: separate fee-rewards watching from general slot watching, such that general slot watching commitment level can be dropped to confirmed
			commitment := rpc.CommitmentFinalized
			epochInfo, err := c.client.GetEpochInfo(ctx_, commitment)
			cancel()
			if err != nil {
				klog.Errorf("Failed to get epoch info, bailing out: %v", err)
				continue
			}

			// if we are running for the first time, then we need to set our tracking numbers:
			if c.currentEpoch == 0 {
				c.trackEpoch(ctx, epochInfo)
			}

			totalTransactionsTotal.Set(float64(epochInfo.TransactionCount))
			confirmedSlotHeight.Set(float64(epochInfo.AbsoluteSlot))

			// if we get here, then the tracking numbers are set, so this is a "normal" run.
			// start by checking if we have progressed since last run:
			if epochInfo.AbsoluteSlot <= c.slotWatermark {
				klog.Infof("%v slot number has not advanced from %v, skipping", commitment, c.slotWatermark)
				continue
			}

			if epochInfo.Epoch > c.currentEpoch {
				// if we have configured inflation reward addresses, fetch em
				if len(c.inflationRewardAddresses) > 0 {
					err = c.fetchAndEmitInflationRewards(ctx, c.currentEpoch)
					if err != nil {
						klog.Errorf("Failed to emit inflation rewards, bailing out: %v", err)
					}
				}
				c.closeCurrentEpoch(ctx, epochInfo)
			}

			// update block production metrics up until the current slot:
			c.moveSlotWatermark(ctx, epochInfo.AbsoluteSlot)
		}
	}
}

// trackEpoch takes in a new rpc.EpochInfo and sets the SlotWatcher tracking metrics accordingly,
// and updates the prometheus gauges associated with those metrics.
func (c *SlotWatcher) trackEpoch(ctx context.Context, epoch *rpc.EpochInfo) {
	klog.Infof("Tracking epoch %v (from %v)", epoch.Epoch, c.currentEpoch)
	firstSlot, lastSlot := getEpochBounds(epoch)
	// if we haven't yet set c.currentEpoch, that (hopefully) means this is the initial setup,
	// and so we can simply store the tracking numbers
	if c.currentEpoch == 0 {
		c.currentEpoch = epoch.Epoch
		c.firstSlot = firstSlot
		c.lastSlot = lastSlot
		// we don't backfill on startup. we set the watermark to current slot minus 1,
		//such that the current slot is the first slot tracked
		c.slotWatermark = epoch.AbsoluteSlot - 1
	} else {
		// if c.currentEpoch is already set, then, just in case, run some checks
		// to make sure that we make sure that we are tracking consistently
		assertf(epoch.Epoch == c.currentEpoch+1, "epoch jumped from %v to %v", c.currentEpoch, epoch.Epoch)
		assertf(
			firstSlot == c.lastSlot+1,
			"first slot %v does not follow from current last slot %v",
			firstSlot,
			c.lastSlot,
		)

		// and also, make sure that we have completed the last epoch:
		assertf(
			c.slotWatermark == c.lastSlot,
			"can't update epoch when watermark %v hasn't reached current last-slot %v",
			c.slotWatermark,
			c.lastSlot,
		)

		// the epoch number is progressing correctly, so we can update our tracking numbers:
		c.currentEpoch = epoch.Epoch
		c.firstSlot = firstSlot
		c.lastSlot = lastSlot
	}

	// emit epoch bounds:
	klog.Infof("Emitting epoch bounds: %v (slots %v -> %v)", c.currentEpoch, c.firstSlot, c.lastSlot)
	currentEpochNumber.Set(float64(c.currentEpoch))
	epochFirstSlot.Set(float64(c.firstSlot))
	epochLastSlot.Set(float64(c.lastSlot))

	// update leader schedule:
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	klog.Infof("Updating leader schedule for epoch %v ...", c.currentEpoch)
	leaderSchedule, err := GetTrimmedLeaderSchedule(
		ctx, c.client, c.feeRewardAddresses, epoch.AbsoluteSlot, c.firstSlot,
	)
	if err != nil {
		klog.Errorf("Failed to get trimmed leader schedule, bailing out: %v", err)
	}
	c.leaderSchedule = leaderSchedule
}

// closeCurrentEpoch is called when an epoch change-over happens, and we need to make sure we track the last
// remaining slots in the "current" epoch before we start tracking the new one.
func (c *SlotWatcher) closeCurrentEpoch(ctx context.Context, newEpoch *rpc.EpochInfo) {
	c.moveSlotWatermark(ctx, c.lastSlot)
	c.trackEpoch(ctx, newEpoch)
}

// checkValidSlotRange makes sure that the slot range we are going to query is within the current epoch we are tracking.
func (c *SlotWatcher) checkValidSlotRange(from, to int64) error {
	if from < c.firstSlot || to > c.lastSlot {
		return fmt.Errorf(
			"start-end slots (%v -> %v) is not contained within current epoch %v range (%v -> %v)",
			from,
			to,
			c.currentEpoch,
			c.firstSlot,
			c.lastSlot,
		)
	}
	return nil
}

// moveSlotWatermark performs all the slot-watching tasks required to move the slotWatermark to the provided 'to' slot.
func (c *SlotWatcher) moveSlotWatermark(ctx context.Context, to int64) {
	c.fetchAndEmitBlockProduction(ctx, to)
	c.fetchAndEmitFeeRewards(ctx, to)
	c.slotWatermark = to
}

// fetchAndEmitBlockProduction fetches block production up to the provided endSlot, emits the prometheus metrics,
// and updates the SlotWatcher.slotWatermark accordingly
func (c *SlotWatcher) fetchAndEmitBlockProduction(ctx context.Context, endSlot int64) {
	// add 1 because GetBlockProduction's range is inclusive, and the watermark is already tracked
	startSlot := c.slotWatermark + 1
	klog.Infof("Fetching block production in [%v -> %v]", startSlot, endSlot)

	// make sure the bounds are contained within the epoch we are currently watching:
	if err := c.checkValidSlotRange(startSlot, endSlot); err != nil {
		klog.Fatalf("invalid slot range: %v", err)
	}

	// fetch block production:
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	blockProduction, err := c.client.GetBlockProduction(ctx, rpc.CommitmentFinalized, nil, &startSlot, &endSlot)
	if err != nil {
		klog.Errorf("Failed to get block production, bailing out: %v", err)
		return
	}

	// emit the metrics:
	for address, production := range blockProduction.ByIdentity {
		valid := float64(production.BlocksProduced)
		skipped := float64(production.LeaderSlots - production.BlocksProduced)

		leaderSlotsTotal.WithLabelValues("valid", address).Add(valid)
		leaderSlotsTotal.WithLabelValues("skipped", address).Add(skipped)

		if len(c.leaderSlotAddresses) == 0 || slices.Contains(c.leaderSlotAddresses, address) {
			epochStr := toString(c.currentEpoch)
			leaderSlotsByEpoch.WithLabelValues("valid", address, epochStr).Add(valid)
			leaderSlotsByEpoch.WithLabelValues("skipped", address, epochStr).Add(skipped)
		}
	}

	klog.Infof("Fetched block production in [%v -> %v]", startSlot, endSlot)
}

// fetchAndEmitFeeRewards fetches and emits all the fee rewards for the tracked addresses between the
// slotWatermark and endSlot
func (c *SlotWatcher) fetchAndEmitFeeRewards(ctx context.Context, endSlot int64) {
	startSlot := c.slotWatermark + 1
	klog.Infof("Fetching fee rewards in [%v -> %v]", startSlot, endSlot)

	if err := c.checkValidSlotRange(startSlot, endSlot); err != nil {
		klog.Fatalf("invalid slot range: %v", err)
	}
	scheduleToFetch := SelectFromSchedule(c.leaderSchedule, startSlot, endSlot)
	for identity, leaderSlots := range scheduleToFetch {
		if len(leaderSlots) == 0 {
			continue
		}

		klog.Infof("Fetching fee rewards for %v in [%v -> %v]: %v ...", identity, startSlot, endSlot, leaderSlots)
		for _, slot := range leaderSlots {
			ctx, cancel := context.WithTimeout(ctx, httpTimeout)
			err := c.fetchAndEmitSingleFeeReward(ctx, identity, c.currentEpoch, slot)
			cancel()
			if err != nil {
				klog.Errorf("Failed to fetch fee rewards for %v at %v: %v", identity, slot, err)
			}
		}
	}

	klog.Infof("Fetched fee rewards in [%v -> %v]", startSlot, endSlot)
}

// fetchAndEmitSingleFeeReward fetches and emits the fee reward for a single block.
func (c *SlotWatcher) fetchAndEmitSingleFeeReward(
	ctx context.Context, identity string, epoch int64, slot int64,
) error {
	block, err := c.client.GetBlock(ctx, rpc.CommitmentConfirmed, slot)
	if err != nil {
		var rpcError *rpc.RPCError
		if errors.As(err, &rpcError) {
			// this is the error code for slot was skipped:
			if rpcError.Code == rpc.SlotSkippedCode && strings.Contains(rpcError.Message, "skipped") {
				klog.Infof("slot %v was skipped, no fee rewards.", slot)
				return nil
			}
		}
		return err
	}

	for _, reward := range block.Rewards {
		if reward.RewardType == "fee" {
			// make sure we haven't made a logic issue or something:
			assertf(
				reward.Pubkey == identity,
				"fetching fee reward for %v but got fee reward for %v",
				identity,
				reward.Pubkey,
			)
			amount := float64(reward.Lamports) / float64(rpc.LamportsInSol)
			feeRewards.WithLabelValues(identity, toString(epoch)).Add(amount)
		}
	}

	return nil
}

// getEpochBounds returns the first slot and last slot within an [inclusive] Epoch
func getEpochBounds(info *rpc.EpochInfo) (int64, int64) {
	firstSlot := info.AbsoluteSlot - info.SlotIndex
	return firstSlot, firstSlot + info.SlotsInEpoch - 1
}

// fetchAndEmitInflationRewards fetches and emits the inflation rewards for the configured inflationRewardAddresses
// at the provided epoch
func (c *SlotWatcher) fetchAndEmitInflationRewards(ctx context.Context, epoch int64) error {
	klog.Infof("Fetching inflation reward for epoch %v ...", toString(epoch))

	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	rewardInfos, err := c.client.GetInflationReward(
		ctx, rpc.CommitmentConfirmed, c.inflationRewardAddresses, &epoch, nil,
	)
	if err != nil {
		return fmt.Errorf("error fetching inflation rewards: %w", err)
	}

	for i, rewardInfo := range rewardInfos {
		address := c.inflationRewardAddresses[i]
		reward := float64(rewardInfo.Amount) / float64(rpc.LamportsInSol)
		inflationRewards.WithLabelValues(address, toString(epoch)).Set(reward)
	}
	klog.Infof("Fetched inflation reward for epoch %v.", epoch)
	return nil
}
