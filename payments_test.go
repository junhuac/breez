package breez

import (
	"io"
	"os"
	"testing"

	"github.com/btcsuite/btclog"
)

const (
	testDir = "./testDir"
)

func copyFile(src, dest string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return
	}
	err = out.Sync()
	return
}

func TestGetPayments(t *testing.T) {
	var err error
	openDB("testDB")
	defer deleteDB()
	payment1 := &paymentInfo{
		Type:              receivedPayment,
		Description:       "Received Payment1",
		Amount:            10,
		CreationTimestamp: 14,
		PaymentHash:       "h1",
	}

	payment2 := &paymentInfo{
		Type:              receivedPayment,
		Description:       "Received Payment2",
		Amount:            10,
		CreationTimestamp: 15,
		PaymentHash:       "h2",
	}
	err = addAccountPayment(payment1, 5, 0)
	if err != nil {
		t.Error("failed to add payment", err)
	}
	err = addAccountPayment(payment2, 4, 0)
	if err != nil {
		t.Error("failed to add payment", err)
	}

	paymentsList, err := GetPayments()
	if err != nil {
		t.Error("Failed to invoke GetPayments", err)
	}
	list := paymentsList.PaymentsList
	if len(list) != 2 {
		t.Error("Payments list should be 2 but instead is ", len(list))
	}

	if list[0].CreationTimestamp != 15 {
		t.Error("First item should have timestamp 15 but instead we got", list[0].CreationTimestamp)
	}
}

func TestMain(m *testing.M) {
	log = btclog.Disabled
	os.Exit(m.Run())
}
