package services

import (
	"eth2-exporter/db"
	"eth2-exporter/mail"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"time"
)

func notificationsSender() {
	for {
		start := time.Now()
		notificationsByEMail := collectNotifications()
		sendNotifications(notificationsByEMail)
		logger.WithField("emails", len(notificationsByEMail)).WithField("duration", time.Since(start)).Info("notifications completed")
		time.Sleep(time.Second * 60)
	}
}

func collectNotifications() map[string]map[types.EventName][]types.Notification {
	notificationsByEmail := map[string]map[types.EventName][]types.Notification{}
	var err error
	err = collectValidatorBalanceDecreasedNotifications(notificationsByEmail)
	if err != nil {
		logger.Errorf("error collecting validator_balance_decreased notifications: %v", err)
	}
	err = collectValidatorGotSlashedNotifications(notificationsByEmail)
	if err != nil {
		logger.Errorf("error collecting validator_got_slashed notifications: %v", err)
	}
	return notificationsByEmail
}

func sendNotifications(notificationsByEmail map[string]map[types.EventName][]types.Notification) {
	for userEmail, userNotifications := range notificationsByEmail {
		go func(userEmail string, userNotifications map[types.EventName][]types.Notification) {
			sentSubsByEpoch := map[uint64][]uint64{}
			subject := fmt.Sprintf("%s: Notification", utils.Config.Frontend.SiteDomain)
			msg := ""
			for event, ns := range userNotifications {
				if len(msg) > 0 {
					msg += "\n"
				}
				msg += fmt.Sprintf("%s\n====\n\n", event)
				for _, n := range ns {
					msg += fmt.Sprintf("%s\n", n.GetInfo())
					e := n.GetEpoch()
					if _, exists := sentSubsByEpoch[e]; !exists {
						sentSubsByEpoch[e] = []uint64{n.GetSubscriptionID()}
					} else {
						sentSubsByEpoch[e] = append(sentSubsByEpoch[e], n.GetSubscriptionID())
					}
				}
			}
			msg += fmt.Sprintf("\nBest regards\n\n%s", utils.Config.Frontend.SiteDomain)

			err := mail.SendMailRateLimited(userEmail, subject, msg)
			if err != nil {
				logger.Errorf("error sending notification-email: %v", err)
				return
			}

			for epoch, subIDs := range sentSubsByEpoch {
				err = db.UpdateSubscriptionsLastSent(subIDs, time.Now(), epoch)
				if err != nil {
					logger.Errorf("error updating sent-time of sent notifications: %v", err)
				}
			}
		}(userEmail, userNotifications)
	}
}

type validatorBalanceDecreasedNotification struct {
	ValidatorIndex     uint64
	ValidatorPublicKey string
	Epoch              uint64
	PrevBalance        uint64
	Balance            uint64
	SubscriptionID     uint64
}

func (n *validatorBalanceDecreasedNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorBalanceDecreasedNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *validatorBalanceDecreasedNotification) GetEventName() types.EventName {
	return types.ValidatorBalanceDecreasedEventName
}

func (n *validatorBalanceDecreasedNotification) GetInfo() string {
	balance := float64(n.Balance) / 1e9
	diff := float64(n.PrevBalance-n.Balance) / 1e9
	return fmt.Sprintf(`The balance of validator %[1]v decreased by %.9[2]f ETH to %.9[3]f ETH at epoch %[4]v. For more information visit: https://%[5]s/validator/%[1]v`, n.ValidatorIndex, diff, balance, n.Epoch, utils.Config.Frontend.SiteDomain)
}

func collectValidatorBalanceDecreasedNotifications(notificationsByEmail map[string]map[types.EventName][]types.Notification) error {
	latestEpoch := LatestEpoch()
	if latestEpoch == 0 {
		return nil
	}
	prevEpoch := latestEpoch - 3

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		Email          string `db:"email"`
		ValidatorIndex uint64 `db:"validatorindex"`
		Balance        uint64 `db:"balance"`
		PrevBalance    uint64 `db:"prevbalance"`
	}
	err := db.DB.Select(&dbResult, `
		WITH
			decreased_balance_validators AS (
				SELECT 
					vb.validatorindex, 
					ENCODE(v.pubkey, 'hex') AS pubkey,
					vb.balance, 
					vb2.balance AS prevbalance
				FROM validator_balances vb
				INNER JOIN validators v ON v.validatorindex = vb.validatorindex
				INNER JOIN validator_balances vb2 ON vb.validatorindex = vb2.validatorindex AND vb2.epoch = $3
				WHERE vb.epoch = $2 AND vb.balance < vb2.balance
			)
		SELECT us.id, u.email, dbv.validatorindex, dbv.balance, dbv.prevbalance
		FROM users_subscriptions us
		INNER JOIN users u ON u.id = us.user_id
		INNER JOIN decreased_balance_validators dbv ON dbv.pubkey = us.event_filter
		WHERE us.event_name = $1 AND us.created_epoch <= $2 AND (us.last_sent_epoch IS NULL OR us.last_sent_epoch < $2)`,
		types.ValidatorBalanceDecreasedEventName, latestEpoch, prevEpoch)
	if err != nil {
		return err
	}

	for _, r := range dbResult {
		n := &validatorBalanceDecreasedNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			Balance:        r.Balance,
			PrevBalance:    r.PrevBalance,
			Epoch:          latestEpoch,
		}

		if _, exists := notificationsByEmail[r.Email]; !exists {
			notificationsByEmail[r.Email] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByEmail[r.Email][n.GetEventName()]; !exists {
			notificationsByEmail[r.Email][n.GetEventName()] = []types.Notification{}
		}
		notificationsByEmail[r.Email][n.GetEventName()] = append(notificationsByEmail[r.Email][n.GetEventName()], n)
	}

	return nil
}

type validatorGotSlashedNotification struct {
	SubscriptionID uint64
	ValidatorIndex uint64
	Epoch          uint64
	Slasher        uint64
	Reason         string
}

func (n *validatorGotSlashedNotification) GetSubscriptionID() uint64 {
	return n.SubscriptionID
}

func (n *validatorGotSlashedNotification) GetEpoch() uint64 {
	return n.Epoch
}

func (n *validatorGotSlashedNotification) GetEventName() types.EventName {
	return types.ValidatorGotSlashedEventName
}

func (n *validatorGotSlashedNotification) GetInfo() string {
	return fmt.Sprintf(`Validator %[1]v has been slashed at epoch %[2]v by validator %[3]v for %[4]s. For more information visit: https://%[5]v/validator/%[1]v`, n.ValidatorIndex, n.Epoch, n.Slasher, n.Reason, utils.Config.Frontend.SiteDomain)
}

func collectValidatorGotSlashedNotifications(notificationsByEmail map[string]map[types.EventName][]types.Notification) error {
	latestEpoch := LatestEpoch()
	if latestEpoch == 0 {
		return nil
	}

	var dbResult []struct {
		SubscriptionID uint64 `db:"id"`
		Email          string `db:"email"`
		ValidatorIndex uint64 `db:"validatorindex"`
		Slasher        uint64 `db:"slasher"`
		Epoch          uint64 `db:"epoch"`
		Reason         string `db:"reason"`
	}
	err := db.DB.Select(&dbResult, `
		WITH
			slashings AS (
				SELECT DISTINCT ON (slashedvalidator) * FROM (
					SELECT
						blocks.slot, 
						blocks.epoch, 
						blocks.proposer AS slasher,
						UNNEST(ARRAY(
							SELECT UNNEST(attestation1_indices)
								INTERSECT
							SELECT UNNEST(attestation2_indices)
						)) AS slashedvalidator,
						'Attestation Violation' AS reason
					FROM blocks_attesterslashings 
					LEFT JOIN blocks ON blocks_attesterslashings.block_slot = blocks.slot
					UNION ALL
						SELECT
							blocks.slot, 
							blocks.epoch, 
							blocks.proposer AS slasher, 
							blocks_proposerslashings.proposerindex AS slashedvalidator,
							'Proposer Violation' AS reason 
						FROM blocks_proposerslashings
						LEFT JOIN blocks ON blocks_proposerslashings.block_slot = blocks.slot
				) a
				ORDER BY slashedvalidator, slot
			)
		SELECT us.id, u.email, v.validatorindex, s.slasher, s.epoch, s.reason
		FROM users_subscriptions us
		INNER JOIN users u ON u.id = us.user_id
		INNER JOIN validators v ON ENCODE(v.pubkey, 'hex') = us.event_filter
		INNER JOIN slashings s ON s.slashedvalidator = v.validatorindex
		WHERE us.event_name = $1 AND us.last_sent_epoch IS NULL AND us.created_epoch < s.epoch`,
		types.ValidatorGotSlashedEventName)
	if err != nil {
		return err
	}

	for _, r := range dbResult {
		n := &validatorGotSlashedNotification{
			SubscriptionID: r.SubscriptionID,
			ValidatorIndex: r.ValidatorIndex,
			Slasher:        r.Slasher,
			Epoch:          r.Epoch,
			Reason:         r.Reason,
		}
		if _, exists := notificationsByEmail[r.Email]; !exists {
			notificationsByEmail[r.Email] = map[types.EventName][]types.Notification{}
		}
		if _, exists := notificationsByEmail[r.Email][n.GetEventName()]; !exists {
			notificationsByEmail[r.Email][n.GetEventName()] = []types.Notification{}
		}
		notificationsByEmail[r.Email][n.GetEventName()] = append(notificationsByEmail[r.Email][n.GetEventName()], n)
	}

	return nil
}
