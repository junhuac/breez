package breez

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"time"

	"github.com/breez/breez/data"
	"github.com/breez/lightninglib/lnrpc"
	"github.com/golang/protobuf/proto"
	"golang.org/x/sync/singleflight"
)

type paymentType byte

const (
	defaultInvoiceExpiry int64 = 3600
	sentPayment                = paymentType(0)
	receivedPayment            = paymentType(1)
	depositPayment             = paymentType(2)
	withdrawalPayment          = paymentType(3)
)

type paymentInfo struct {
	Type                       paymentType
	Amount                     int64
	CreationTimestamp          int64
	Description                string
	PayeeName                  string
	PayeeImageURL              string
	PayerName                  string
	PayerImageURL              string
	TransferRequest            bool
	PaymentHash                string
	RedeemTxID                 string
	Destination                string
	PendingExpirationHeight    uint32
	PendingExpirationTimestamp int64
}

func serializePaymentInfo(s *paymentInfo) ([]byte, error) {
	return json.Marshal(s)
}

func deserializePaymentInfo(paymentBytes []byte) (*paymentInfo, error) {
	var payment paymentInfo
	err := json.Unmarshal(paymentBytes, &payment)
	return &payment, err
}

var blankInvoiceGroup singleflight.Group

/*
GetPayments is responsible for retrieving the payment were made in this account
*/
func GetPayments() (*data.PaymentsList, error) {
	rawPayments, err := fetchAllAccountPayments()
	if err != nil {
		return nil, err
	}

	pendingPayments, err := getPendingPayments()
	if err != nil {
		return nil, err
	}
	rawPayments = append(rawPayments, pendingPayments...)

	var paymentsList []*data.Payment
	for _, payment := range rawPayments {
		paymentItem := &data.Payment{
			Amount:            payment.Amount,
			CreationTimestamp: payment.CreationTimestamp,
			RedeemTxID:        payment.RedeemTxID,
			PaymentHash:       payment.PaymentHash,
			Destination:       payment.Destination,
			InvoiceMemo: &data.InvoiceMemo{
				Description:     payment.Description,
				Amount:          payment.Amount,
				PayeeImageURL:   payment.PayeeImageURL,
				PayeeName:       payment.PayeeName,
				PayerImageURL:   payment.PayerImageURL,
				PayerName:       payment.PayerName,
				TransferRequest: payment.TransferRequest,
			},
			PendingExpirationHeight:    payment.PendingExpirationHeight,
			PendingExpirationTimestamp: payment.PendingExpirationTimestamp,
		}
		switch payment.Type {
		case sentPayment:
			paymentItem.Type = data.Payment_SENT
		case receivedPayment:
			paymentItem.Type = data.Payment_RECEIVED
		case depositPayment:
			paymentItem.Type = data.Payment_DEPOSIT
		case withdrawalPayment:
			paymentItem.Type = data.Payment_WITHDRAWAL
		}

		paymentsList = append(paymentsList, paymentItem)
	}

	sort.Slice(paymentsList, func(i, j int) bool {
		return paymentsList[i].CreationTimestamp > paymentsList[j].CreationTimestamp
	})

	resultPayments := &data.PaymentsList{PaymentsList: paymentsList}
	return resultPayments, nil
}

/*
SendPaymentForRequest send the payment according to the details specified in the bolt 11 payment request.
If the payment was failed an error is returned
*/
func SendPaymentForRequest(paymentRequest string, amountSatoshi int64) error {
	log.Infof("sendPaymentForRequest: amount = %v", amountSatoshi)
	decodedReq, err := lightningClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		return err
	}
	if err := savePaymentRequest(decodedReq.PaymentHash, []byte(paymentRequest)); err != nil {
		return err
	}
	log.Infof("sendPaymentForRequest: before sending payment...")
	response, err := lightningClient.SendPaymentSync(context.Background(), &lnrpc.SendRequest{PaymentRequest: paymentRequest, Amt: amountSatoshi})
	if err != nil {
		log.Infof("sendPaymentForRequest: error sending payment %v", err)
		return err
	}
	log.Infof("sendPaymentForRequest finished successfully")
	if len(response.PaymentError) > 0 {
		return errors.New(response.PaymentError)
	}

	syncSentPayments()
	return nil
}

/*
AddInvoice encapsulate a given invoice information in a payment request
*/
func AddInvoice(invoice *data.InvoiceMemo) (paymentRequest string, err error) {
	memo, err := proto.Marshal(invoice)
	if err != nil {
		return "", err
	}

	var invoiceExpiry int64
	if invoice.Expiry <= 0 {
		invoiceExpiry = defaultInvoiceExpiry
	} else {
		invoiceExpiry = invoice.Expiry
	}

	response, err := lightningClient.AddInvoice(context.Background(), &lnrpc.Invoice{Memo: string(memo), Private: true, Value: invoice.Amount, Expiry: invoiceExpiry})
	if err != nil {
		return "", err
	}
	log.Infof("Generated Invoice: %v", response.PaymentRequest)
	return response.PaymentRequest, nil
}

/*
AddStandardInvoice encapsulate a given amount and description in a payment request
*/
func AddStandardInvoice(invoice *data.InvoiceMemo) (paymentRequest string, err error) {
	// Format the standard invoice memo
	memo := invoice.Description + " | " + invoice.PayeeName + " | " + invoice.PayeeImageURL

	if invoice.Expiry <= 0 {
		invoice.Expiry = defaultInvoiceExpiry
	}

	response, err := lightningClient.AddInvoice(context.Background(), &lnrpc.Invoice{Memo: memo, Private: true, Value: invoice.Amount, Expiry: invoice.Expiry})
	if err != nil {
		return "", err
	}
	log.Infof("Generated Invoice: %v", response.PaymentRequest)
	return response.PaymentRequest, nil
}

/*
DecodeInvoice is used by the payer to decode the payment request and read the invoice details.
*/
func DecodePaymentRequest(paymentRequest string) (*data.InvoiceMemo, error) {
	log.Infof("DecodePaymentRequest %v", paymentRequest)
	decodedPayReq, err := lightningClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		log.Errorf("DecodePaymentRequest error: %v", err)
		return nil, err
	}
	invoiceMemo := &data.InvoiceMemo{}
	if err := proto.Unmarshal([]byte(decodedPayReq.Description), invoiceMemo); err != nil {
		// In case we cannot unmarshal the description we are probably dealing with a standard invoice
		if strings.Count(decodedPayReq.Description, " | ") == 2 {
			// There is also the 'description | payee | logo' encoding
			// meant to encode breez metadata in a way that's human readable
			invoiceData := strings.Split(decodedPayReq.Description, " | ")
			invoiceMemo.Description = invoiceData[0]
			invoiceMemo.PayeeName = invoiceData[1]
			invoiceMemo.PayeeImageURL = invoiceData[2]
		} else {
			invoiceMemo.Description = decodedPayReq.Description
		}
		invoiceMemo.Amount = decodedPayReq.NumSatoshis
	}

	return invoiceMemo, nil
}

/*
GetRelatedInvoice is used by the payee to fetch the related invoice of its sent payment request so he can see if it is settled.
*/
func GetRelatedInvoice(paymentRequest string) (*data.Invoice, error) {
	decodedPayReq, err := lightningClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: paymentRequest})
	if err != nil {
		return nil, err
	}

	invoiceMemo := &data.InvoiceMemo{}
	if err := proto.Unmarshal([]byte(decodedPayReq.Description), invoiceMemo); err != nil {
		return nil, err
	}

	lookup, err := lightningClient.LookupInvoice(context.Background(), &lnrpc.PaymentHash{RHashStr: decodedPayReq.PaymentHash})
	if err != nil {
		return nil, err
	}

	invoice := &data.Invoice{
		Memo:    invoiceMemo,
		AmtPaid: lookup.AmtPaidSat,
		Settled: lookup.Settled,
	}

	return invoice, nil
}

func watchPayments() {
	syncSentPayments()
	_, lastInvoiceSettledIndex := fetchPaymentsSyncInfo()
	log.Infof("last invoice settled index ", lastInvoiceSettledIndex)
	stream, err := lightningClient.SubscribeInvoices(context.Background(), &lnrpc.InvoiceSubscription{SettleIndex: lastInvoiceSettledIndex})
	if err != nil {
		log.Criticalf("Failed to call SubscribeInvoices %v, %v", stream, err)
	}

	go func() {
		for {
			invoice, err := stream.Recv()
			log.Infof("watchPayments - Invoice received by subscription")
			if err != nil {
				log.Criticalf("Failed to receive an invoice : %v", err)
				return
			}
			if invoice.Settled {
				log.Infof("watchPayments adding a received payment")
				if err = onNewReceivedPayment(invoice); err != nil {
					log.Criticalf("Failed to update received payment : %v", err)
					return
				}
			}
		}
	}()
}

func syncSentPayments() error {
	log.Infof("syncSentPayments")
	lightningPayments, err := lightningClient.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{})
	if err != nil {
		return err
	}
	lastPaymentTime, _ := fetchPaymentsSyncInfo()
	for _, paymentItem := range lightningPayments.Payments {
		if paymentItem.CreationDate <= lastPaymentTime {
			continue
		}
		log.Infof("syncSentPayments adding an outgoing payment")
		onNewSentPayment(paymentItem)
	}

	return nil

	//TODO delete history of payment requests after the new payments API stablized.
}

func getPendingPayments() ([]*paymentInfo, error) {
	var payments []*paymentInfo

	if DaemonReady() {
		channelsRes, err := lightningClient.ListChannels(context.Background(), &lnrpc.ListChannelsRequest{})
		if err != nil {
			return nil, err
		}

		chainInfo, chainErr := lightningClient.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
		if chainErr != nil {
			log.Errorf("Failed get chain info", chainErr)
			return nil, chainErr
		}

		for _, ch := range channelsRes.Channels {
			for _, htlc := range ch.PendingHtlcs {
				pendingItem, err := createPendingPayment(htlc, chainInfo.BlockHeight)
				if err != nil {
					return nil, err
				}
				payments = append(payments, pendingItem)
			}
		}
	}

	return payments, nil
}

func createPendingPayment(htlc *lnrpc.HTLC, currentBlockHeight uint32) (*paymentInfo, error) {
	paymentType := sentPayment
	if htlc.Incoming {
		paymentType = receivedPayment
	}

	var paymentRequest string
	if htlc.Incoming {
		invoice, err := lightningClient.LookupInvoice(context.Background(), &lnrpc.PaymentHash{RHash: htlc.HashLock})
		if err != nil {
			log.Errorf("createPendingPayment - failed to call LookupInvoice %v", err)
			return nil, err
		}
		paymentRequest = invoice.PaymentRequest
	} else {
		payReqBytes, err := fetchPaymentRequest(string(htlc.HashLock))
		if err != nil {
			log.Errorf("createPendingPayment - failed to call fetchPaymentRequest %v", err)
			return nil, err
		}
		paymentRequest = string(payReqBytes)
	}

	minutesToExpire := time.Duration((htlc.ExpirationHeight - currentBlockHeight) * 10)
	paymentData := &paymentInfo{
		Type:                       paymentType,
		Amount:                     htlc.Amount,
		CreationTimestamp:          time.Now().Unix(),
		PendingExpirationHeight:    htlc.ExpirationHeight,
		PendingExpirationTimestamp: time.Now().Add(minutesToExpire * time.Minute).Unix(),
	}

	if paymentRequest != "" {
		decodedReq, err := lightningClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: paymentRequest})
		if err != nil {
			return nil, err
		}

		invoiceMemo, err := DecodePaymentRequest(paymentRequest)
		if err != nil {
			return nil, err
		}

		paymentData.Description = invoiceMemo.Description
		paymentData.PayeeImageURL = invoiceMemo.PayeeImageURL
		paymentData.PayeeName = invoiceMemo.PayeeName
		paymentData.PayerImageURL = invoiceMemo.PayerImageURL
		paymentData.PayerName = invoiceMemo.PayerName
		paymentData.TransferRequest = invoiceMemo.TransferRequest
		paymentData.PaymentHash = decodedReq.PaymentHash
		paymentData.Destination = decodedReq.Destination
		paymentData.CreationTimestamp = decodedReq.Timestamp
	}

	return paymentData, nil
}

func onNewSentPayment(paymentItem *lnrpc.Payment) error {
	paymentRequest, err := fetchPaymentRequest(paymentItem.PaymentHash)
	if err != nil {
		return err
	}
	var invoiceMemo *data.InvoiceMemo
	if paymentRequest != nil && len(paymentRequest) > 0 {
		if invoiceMemo, err = DecodePaymentRequest(string(paymentRequest)); err != nil {
			return err
		}
	}

	paymentType := sentPayment
	decodedReq, err := lightningClient.DecodePayReq(context.Background(), &lnrpc.PayReqString{PayReq: string(paymentRequest)})
	if err != nil {
		return err
	}
	if decodedReq.Destination == cfg.RoutingNodePubKey {
		paymentType = withdrawalPayment
	}

	paymentData := &paymentInfo{
		Type:              paymentType,
		Amount:            paymentItem.Value,
		CreationTimestamp: paymentItem.CreationDate,
		Description:       invoiceMemo.Description,
		PayeeImageURL:     invoiceMemo.PayeeImageURL,
		PayeeName:         invoiceMemo.PayeeName,
		PayerImageURL:     invoiceMemo.PayerImageURL,
		PayerName:         invoiceMemo.PayerName,
		TransferRequest:   invoiceMemo.TransferRequest,
		PaymentHash:       decodedReq.PaymentHash,
		Destination:       decodedReq.Destination,
	}

	err = addAccountPayment(paymentData, 0, uint64(paymentItem.CreationDate))
	go func() {
		time.Sleep(2 * time.Second)
		extractBackupPaths()
	}()
	onAccountChanged()
	return err
}

func onNewReceivedPayment(invoice *lnrpc.Invoice) error {
	var invoiceMemo *data.InvoiceMemo
	var err error
	if len(invoice.PaymentRequest) > 0 {
		if invoiceMemo, err = DecodePaymentRequest(invoice.PaymentRequest); err != nil {
			return err
		}
	}

	paymentType := receivedPayment
	if invoiceMemo.TransferRequest {
		paymentType = depositPayment
	}

	paymentData := &paymentInfo{
		Type:              paymentType,
		Amount:            invoice.AmtPaidSat,
		CreationTimestamp: invoice.SettleDate,
		Description:       invoiceMemo.Description,
		PayeeImageURL:     invoiceMemo.PayeeImageURL,
		PayeeName:         invoiceMemo.PayeeName,
		PayerImageURL:     invoiceMemo.PayerImageURL,
		PayerName:         invoiceMemo.PayerName,
		TransferRequest:   invoiceMemo.TransferRequest,
		PaymentHash:       hex.EncodeToString(invoice.RHash),
	}

	err = addAccountPayment(paymentData, invoice.SettleIndex, 0)
	if err != nil {
		log.Criticalf("Unable to add reveived payment : %v", err)
		return err
	}
	notificationsChan <- data.NotificationEvent{Type: data.NotificationEvent_INVOICE_PAID}
	go func() {
		time.Sleep(2 * time.Second)
		extractBackupPaths()
	}()
	onAccountChanged()
	return nil
}
