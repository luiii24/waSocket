package waSocket

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/protocol"
	"google.golang.org/protobuf/proto"

	waBinary "go.amirul.dev/waSocket/binary"
	waProto "go.amirul.dev/waSocket/binary/proto"
	"go.amirul.dev/waSocket/types"
	"go.amirul.dev/waSocket/types/events"
)

// Number of sent messages to cache in memory for handling retry receipts.
const recentMessagesSize = 256

type recentMessageKey struct {
	To types.JID
	ID types.MessageID
}

// RecentMessage contains the info needed to re-send a message when another device fails to decrypt it.
type RecentMessage struct {
	Proto     *waProto.Message
	Timestamp time.Time
}

func (cli *Client) addRecentMessage(to types.JID, id types.MessageID, message *waProto.Message) {
	cli.recentMessagesLock.Lock()
	key := recentMessageKey{to, id}
	if cli.recentMessagesList[cli.recentMessagesPtr].ID != "" {
		delete(cli.recentMessagesMap, cli.recentMessagesList[cli.recentMessagesPtr])
	}
	cli.recentMessagesMap[key] = message
	cli.recentMessagesList[cli.recentMessagesPtr] = key
	cli.recentMessagesPtr++
	if cli.recentMessagesPtr >= len(cli.recentMessagesList) {
		cli.recentMessagesPtr = 0
	}
	cli.recentMessagesLock.Unlock()
}

func (cli *Client) getRecentMessage(to types.JID, id types.MessageID) *waProto.Message {
	cli.recentMessagesLock.RLock()
	msg, _ := cli.recentMessagesMap[recentMessageKey{to, id}]
	cli.recentMessagesLock.RUnlock()
	return msg
}

func (cli *Client) getMessageForRetry(receipt *events.Receipt, messageID types.MessageID) (*waProto.Message, error) {
	msg := cli.getRecentMessage(receipt.Chat, messageID)
	if msg == nil {
		msg = cli.GetMessageForRetry(receipt.Sender, receipt.Chat, messageID)
		if msg == nil {
			return nil, fmt.Errorf("couldn't find message %s", messageID)
		} else {
			cli.Log.Debugf("Found message in GetMessageForRetry to accept retry receipt for %s/%s from %s", receipt.Chat, messageID, receipt.Sender)
		}
	} else {
		cli.Log.Debugf("Found message in local cache to accept retry receipt for %s/%s from %s", receipt.Chat, messageID, receipt.Sender)
	}
	return proto.Clone(msg).(*waProto.Message), nil
}

const recreateSessionTimeout = 1 * time.Hour

func (cli *Client) shouldRecreateSession(retryCount int, jid types.JID) (reason string, recreate bool) {
	cli.sessionRecreateHistoryLock.Lock()
	defer cli.sessionRecreateHistoryLock.Unlock()
	if !cli.Store.ContainsSession(jid.SignalAddress()) {
		cli.sessionRecreateHistory[jid] = time.Now()
		return "we don't have a Signal session with them", true
	} else if retryCount < 2 {
		return "", false
	}
	prevTime, ok := cli.sessionRecreateHistory[jid]
	if !ok || prevTime.Add(recreateSessionTimeout).Before(time.Now()) {
		cli.sessionRecreateHistory[jid] = time.Now()
		return "retry count > 1 and over an hour since last recreation", true
	}
	return "", false
}

// handleRetryReceipt handles an incoming retry receipt for an outgoing message.
func (cli *Client) handleRetryReceipt(receipt *events.Receipt, node *waBinary.Node) error {
	retryChild, ok := node.GetOptionalChildByTag("retry")
	if !ok {
		return &ElementMissingError{Tag: "retry", In: "retry receipt"}
	}
	ag := retryChild.AttrGetter()
	messageID := ag.String("id")
	timestamp := ag.UnixTime("t")
	retryCount := ag.Int("count")
	if !ag.OK() {
		return ag.Error()
	}
	msg, err := cli.getMessageForRetry(receipt, messageID)
	if err != nil {
		return err
	}
	ownID := cli.getOwnID()
	if ownID.IsEmpty() {
		return ErrNotLoggedIn
	}

	if receipt.IsGroup {
		builder := groups.NewGroupSessionBuilder(cli.Store, pbSerializer)
		senderKeyName := protocol.NewSenderKeyName(receipt.Chat.String(), ownID.SignalAddress())
		signalSKDMessage, err := builder.Create(senderKeyName)
		if err != nil {
			cli.Log.Warnf("Failed to create sender key distribution message to include in retry of %s in %s to %s: %v", messageID, receipt.Chat, receipt.Sender, err)
		} else {
			msg.SenderKeyDistributionMessage = &waProto.SenderKeyDistributionMessage{
				GroupId:                             proto.String(receipt.Chat.String()),
				AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
			}
		}
	} else if receipt.IsFromMe {
		msg = &waProto.Message{
			DeviceSentMessage: &waProto.DeviceSentMessage{
				DestinationJid: proto.String(receipt.Chat.String()),
				Message:        msg,
			},
		}
	}

	if cli.PreRetryCallback != nil && !cli.PreRetryCallback(receipt, messageID, retryCount, msg) {
		cli.Log.Debugf("Cancelled retry receipt in PreRetryCallback")
		return nil
	}

	plaintext, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	_, hasKeys := node.GetOptionalChildByTag("keys")
	var bundle *prekey.Bundle
	if hasKeys {
		bundle, err = nodeToPreKeyBundle(uint32(receipt.Sender.Device), *node)
		if err != nil {
			return fmt.Errorf("failed to read prekey bundle in retry receipt: %w", err)
		}
	} else if reason, recreate := cli.shouldRecreateSession(retryCount, receipt.Sender); recreate {
		cli.Log.Debugf("Fetching prekeys for %s for handling retry receipt with no prekey bundle because %s", receipt.Sender, reason)
		var keys map[types.JID]preKeyResp
		keys, err = cli.fetchPreKeys(context.TODO(), []types.JID{receipt.Sender})
		if err != nil {
			return err
		}
		bundle, err = keys[receipt.Sender].bundle, keys[receipt.Sender].err
		if err != nil {
			return fmt.Errorf("failed to fetch prekeys: %w", err)
		} else if bundle == nil {
			return fmt.Errorf("didn't get prekey bundle for %s (response size: %d)", receipt.Sender, len(keys))
		}
	}
	encAttrs := waBinary.Attrs{}
	if mediaType := getMediaTypeFromMessage(msg); mediaType != "" {
		encAttrs["mediatype"] = mediaType
	}
	encrypted, includeDeviceIdentity, err := cli.encryptMessageForDevice(plaintext, receipt.Sender, bundle, encAttrs)
	if err != nil {
		return fmt.Errorf("failed to encrypt message for retry: %w", err)
	}
	encrypted.Attrs["count"] = retryCount

	attrs := waBinary.Attrs{
		"to":   node.Attrs["from"],
		"type": getTypeFromMessage(msg),
		"id":   messageID,
		"t":    timestamp.Unix(),
	}
	if !receipt.IsGroup {
		attrs["device_fanout"] = false
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if edit, ok := node.Attrs["edit"]; ok {
		attrs["edit"] = edit
	}
	err = cli.sendNode(waBinary.Node{
		Tag:     "message",
		Attrs:   attrs,
		Content: cli.getMessageContent(*encrypted, msg, attrs, includeDeviceIdentity),
	})
	if err != nil {
		return fmt.Errorf("failed to send retry message: %w", err)
	}
	cli.Log.Debugf("Sent retry #%d for %s/%s to %s", retryCount, receipt.Chat, messageID, receipt.Sender)
	return nil
}

func (cli *Client) cancelDelayedRequestFromPhone(msgID types.MessageID) {
	if !cli.AutomaticMessageRerequestFromPhone {
		return
	}
	cli.pendingPhoneRerequestsLock.RLock()
	cancelPendingRequest, ok := cli.pendingPhoneRerequests[msgID]
	if ok {
		cancelPendingRequest()
	}
	cli.pendingPhoneRerequestsLock.RUnlock()
}

// RequestFromPhoneDelay specifies how long to wait for the sender to resend the message before requesting from your phone.
// This is only used if Client.AutomaticMessageRerequestFromPhone is true.
var RequestFromPhoneDelay = 5 * time.Second

func (cli *Client) delayedRequestMessageFromPhone(info *types.MessageInfo) {
	if !cli.AutomaticMessageRerequestFromPhone {
		return
	}
	cli.pendingPhoneRerequestsLock.Lock()
	_, alreadyRequesting := cli.pendingPhoneRerequests[info.ID]
	if alreadyRequesting {
		cli.pendingPhoneRerequestsLock.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli.pendingPhoneRerequests[info.ID] = cancel
	cli.pendingPhoneRerequestsLock.Unlock()

	defer func() {
		cli.pendingPhoneRerequestsLock.Lock()
		delete(cli.pendingPhoneRerequests, info.ID)
		cli.pendingPhoneRerequestsLock.Unlock()
	}()
	select {
	case <-time.After(RequestFromPhoneDelay):
	case <-ctx.Done():
		cli.Log.Debugf("Cancelled delayed request for message %s from phone", info.ID)
		return
	}
	_, err := cli.SendMessage(
		ctx,
		cli.getOwnID().ToNonAD(),
		cli.BuildUnavailableMessageRequest(info.Chat, info.Sender, info.ID),
		SendRequestExtra{Peer: true},
	)
	if err != nil {
		cli.Log.Warnf("Failed to send request for unavailable message %s to phone: %v", info.ID, err)
	} else {
		cli.Log.Debugf("Requested message %s from phone", info.ID)
	}
}

// sendRetryReceipt sends a retry receipt for an incoming message.
func (cli *Client) sendRetryReceipt(node *waBinary.Node, info *types.MessageInfo, forceIncludeIdentity bool) {
	id, _ := node.Attrs["id"].(string)
	children := node.GetChildren()
	var retryCountInMsg int
	if len(children) == 1 && children[0].Tag == "enc" {
		retryCountInMsg = children[0].AttrGetter().OptionalInt("count")
	}

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	// In case the message is a retry response, and we restarted in between, find the count from the message
	if retryCount == 1 && retryCountInMsg > 0 {
		retryCount = retryCountInMsg + 1
		cli.messageRetries[id] = retryCount
	}
	cli.messageRetriesLock.Unlock()
	if retryCount >= 5 {
		cli.Log.Warnf("Not sending any more retry receipts for %s", id)
		return
	}
	if retryCount == 1 {
		go cli.delayedRequestMessageFromPhone(info)
	}

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint32(registrationIDBytes[:], cli.Store.RegistrationID)
	attrs := waBinary.Attrs{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: waBinary.Attrs{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 || forceIncludeIdentity {
		if key, err := cli.Store.PreKeys.GenOnePreKey(); err != nil {
			cli.Log.Errorf("Failed to get prekey for retry receipt: %v", err)
		} else if deviceIdentity, err := proto.Marshal(cli.Store.Account); err != nil {
			cli.Log.Errorf("Failed to marshal account info: %v", err)
			return
		} else {
			payload.Content = append(payload.GetChildren(), waBinary.Node{
				Tag: "keys",
				Content: []waBinary.Node{
					{Tag: "type", Content: []byte{ecc.DjbType}},
					{Tag: "identity", Content: cli.Store.IdentityKey.Pub[:]},
					preKeyToNode(key),
					preKeyToNode(cli.Store.SignedPreKey),
					{Tag: "device-identity", Content: deviceIdentity},
				},
			})
		}
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorf("Failed to send retry receipt for %s: %v", id, err)
	}
}
