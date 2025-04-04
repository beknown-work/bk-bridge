//go:build whatsappmulti
// +build whatsappmulti

package bwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	goproto "google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite" // needed for sqlite
)

const (
	// Account config parameters
	cfgNumber = "Number"
)

// Bwhatsapp Bridge structure keeping all the information needed for relying
type Bwhatsapp struct {
	*bridge.Config

	startedAt    time.Time
	wc           *whatsmeow.Client
	contacts     map[types.JID]types.ContactInfo
	users        map[string]types.ContactInfo
	userAvatars  map[string]string
	joinedGroups []*types.GroupInfo
}

type Replyable struct {
	MessageID types.MessageID
	Sender    types.JID
}

// New Create a new WhatsApp bridge. This will be called for each [whatsapp.<server>] entry you have in the config file
func New(cfg *bridge.Config) bridge.Bridger {
	number := cfg.GetString(cfgNumber)

	if number == "" {
		cfg.Log.Fatalf("Missing configuration for WhatsApp bridge: Number")
	}

	b := &Bwhatsapp{
		Config: cfg,

		users:       make(map[string]types.ContactInfo),
		userAvatars: make(map[string]string),
	}

	return b
}

// Connect to WhatsApp. Required implementation of the Bridger interface
func (b *Bwhatsapp) Connect() error {
	device, err := b.getDevice()
	if err != nil {
		return err
	}

	number := b.GetString(cfgNumber)
	if number == "" {
		return errors.New("whatsapp's telephone number need to be configured")
	}

	b.Log.Debugln("Connecting to WhatsApp..")

	b.wc = whatsmeow.NewClient(device, waLog.Stdout("Client", "INFO", true))
	b.wc.AddEventHandler(b.eventHandler)

	firstlogin := false
	var qrChan <-chan whatsmeow.QRChannelItem
	if b.wc.Store.ID == nil {
		firstlogin = true
		qrChan, err = b.wc.GetQRChannel(context.Background())
		if err != nil && !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			return errors.New("failed to to get QR channel:" + err.Error())
		}
	}

	err = b.wc.Connect()
	if err != nil {
		return errors.New("failed to connect to WhatsApp: " + err.Error())
	}

	if b.wc.Store.ID == nil {
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				b.Log.Infof("QR channel result: %s", evt.Event)
			}
		}
	}

	// disconnect and reconnect on our first login/pairing
	// for some reason the GetJoinedGroups in JoinChannel doesn't work on first login
	if firstlogin {
		b.wc.Disconnect()
		time.Sleep(time.Second)

		err = b.wc.Connect()
		if err != nil {
			return errors.New("failed to connect to WhatsApp: " + err.Error())
		}
	}

	b.Log.Infoln("WhatsApp connection successful")

	b.contacts, err = b.wc.Store.Contacts.GetAllContacts()
	if err != nil {
		return errors.New("failed to get contacts: " + err.Error())
	}

	b.joinedGroups, err = b.wc.GetJoinedGroups()
	if err != nil {
		return errors.New("failed to get list of joined groups: " + err.Error())
	}

	b.startedAt = time.Now()

	// map all the users
	for id, contact := range b.contacts {
		if !isGroupJid(id.String()) && id.String() != "status@broadcast" {
			// it is user
			b.users[id.String()] = contact
		}
	}

	// get user avatar asynchronously
	// b.Log.Info("Getting user avatars..")

	// for jid := range b.users {
	// 	info, err := b.GetProfilePicThumb(jid)
	// 	if err != nil {
	// 		b.Log.Warnf("Could not get profile photo of %s: %v", jid, err)
	// 	} else {
	// 		b.Lock()
	// 		if info != nil {
	// 			b.userAvatars[jid] = info.URL
	// 		}
	// 		b.Unlock()
	// 	}
	// }

	b.Log.Info("Finished getting avatars..")

	return nil
}

// Disconnect is called while reconnecting to the bridge
// Required implementation of the Bridger interface
func (b *Bwhatsapp) Disconnect() error {
	b.wc.Disconnect()

	return nil
}

// JoinChannel Join a WhatsApp group specified in gateway config as channel='number-id@g.us' or channel='Channel name'
// Required implementation of the Bridger interface
// https://github.com/42wim/matterbridge/blob/2cfd880cdb0df29771bf8f31df8d990ab897889d/bridge/bridge.go#L11-L16
func (b *Bwhatsapp) JoinChannel(channel config.ChannelInfo) error {
	byJid := isGroupJid(channel.Name)

	// verify if we are member of the given group
	if byJid {
		gJID, err := types.ParseJID(channel.Name)
		if err != nil {
			return err
		}

		for _, group := range b.joinedGroups {
			if group.JID == gJID {
				return nil
			}
		}
	}

	foundGroups := []string{}

	for _, group := range b.joinedGroups {
		if group.Name == channel.Name {
			foundGroups = append(foundGroups, group.Name)
		}
	}

	switch len(foundGroups) {
	case 0:
		// didn't match any group - print out possibilites
		for _, group := range b.joinedGroups {
			b.Log.Infof("%s %s", group.JID, group.Name)
		}
		return fmt.Errorf("please specify group's JID from the list above instead of the name '%s'", channel.Name)
	case 1:
		return fmt.Errorf("group name might change. Please configure gateway with channel=\"%v\" instead of channel=\"%v\"", foundGroups[0], channel.Name)
	default:
		return fmt.Errorf("there is more than one group with name '%s'. Please specify one of JIDs as channel name: %v", channel.Name, foundGroups)
	}
}

// Post a document message from the bridge to WhatsApp
func (b *Bwhatsapp) PostDocumentMessage(msg config.Message, filetype string) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment
	mentionedJids := b.parseMentions(caption) // Parse mentions from caption

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaDocument)
	if err != nil {
		return "", err
	}

	// Post document message
	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID) // Error ignored for simplicity, consider handling
	}

	// Ensure context exists if we have mentions
	if len(mentionedJids) > 0 {
		if ctx == nil {
			ctx = &proto.ContextInfo{}
		}
		ctx.MentionedJID = mentionedJids
	}

	message.DocumentMessage = &proto.DocumentMessage{
		Title:         &fi.Name,
		FileName:      &fi.Name,
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    goproto.Uint64(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as a document", msg)

	ID := whatsmeow.GenerateMessageID()
	_, err = b.wc.SendMessage(context.TODO(), groupJID, &message, whatsmeow.SendRequestExtra{ID: ID})

	return ID, err
}

// Post an image message from the bridge to WhatsApp
// Handle, for sure image/jpeg, image/png and image/gif MIME types
func (b *Bwhatsapp) PostImageMessage(msg config.Message, filetype string) (string, error) {
	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment
	mentionedJids := b.parseMentions(caption) // Parse mentions from caption

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaImage)
	if err != nil {
		return "", err
	}

	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID) // Error ignored for simplicity, consider handling
	}

	// Ensure context exists if we have mentions
	if len(mentionedJids) > 0 {
		if ctx == nil {
			ctx = &proto.ContextInfo{}
		}
		ctx.MentionedJID = mentionedJids
	}

	message.ImageMessage = &proto.ImageMessage{
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    goproto.Uint64(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as an image", msg)

	return b.sendMessage(msg, &message)
}

// Post a video message from the bridge to WhatsApp
func (b *Bwhatsapp) PostVideoMessage(msg config.Message, filetype string) (string, error) {
	fi := msg.Extra["file"][0].(config.FileInfo)

	caption := msg.Username + fi.Comment
	mentionedJids := b.parseMentions(caption) // Parse mentions from caption

	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaVideo)
	if err != nil {
		return "", err
	}

	var message proto.Message
	var ctx *proto.ContextInfo
	if msg.ParentID != "" {
		ctx, _ = b.getNewReplyContext(msg.ParentID) // Error ignored for simplicity, consider handling
	}

	// Ensure context exists if we have mentions
	if len(mentionedJids) > 0 {
		if ctx == nil {
			ctx = &proto.ContextInfo{}
		}
		ctx.MentionedJID = mentionedJids
	}

	message.VideoMessage = &proto.VideoMessage{
		Mimetype:      &filetype,
		Caption:       &caption,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    goproto.Uint64(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   ctx,
	}

	b.Log.Debugf("=> Sending %#v as a video", msg)

	return b.sendMessage(msg, &message)
}

// Post audio inline
func (b *Bwhatsapp) PostAudioMessage(msg config.Message, filetype string) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	fi := msg.Extra["file"][0].(config.FileInfo)

	// Upload audio
	resp, err := b.wc.Upload(context.Background(), *fi.Data, whatsmeow.MediaAudio)
	if err != nil {
		return "", err
	}

	var audioMessage proto.Message
	var audioCtx *proto.ContextInfo
	if msg.ParentID != "" {
		// Apply reply context ONLY to the audio message itself, not the caption.
		// Mentions will be handled in the separate caption message.
		audioCtx, _ = b.getNewReplyContext(msg.ParentID) // Error ignored for simplicity, consider handling
	}

	audioMessage.AudioMessage = &proto.AudioMessage{
		Mimetype:      &filetype,
		MediaKey:      resp.MediaKey,
		FileEncSHA256: resp.FileEncSHA256,
		FileSHA256:    resp.FileSHA256,
		FileLength:    goproto.Uint64(resp.FileLength),
		URL:           &resp.URL,
		DirectPath:    &resp.DirectPath,
		ContextInfo:   audioCtx, // Use context only for reply here
	}

	b.Log.Debugf("=> Sending %#v as audio", msg)

	// Send the audio message
	audioMsgID, err := b.sendMessage(msg, &audioMessage)
	if err != nil {
		b.Log.Errorf("Failed to send audio message part: %v", err)
		// Decide if we should still try to send the caption or return here
		// return "", err // Option: return error immediately
	}

	// Prepare and send the caption message separately, including mentions
	caption := msg.Username + fi.Comment
	if caption != "" { // Only send caption if it's not empty
		caption += "\n\u2B06" // the char on the end is upwards arrow emoji
		mentionedJids := b.parseMentions(caption)

		var captionMessage proto.Message
		var captionCtx *proto.ContextInfo // Separate context for the caption message

		// If there are mentions, we MUST use ExtendedTextMessage
		if len(mentionedJids) > 0 {
			captionCtx = &proto.ContextInfo{MentionedJID: mentionedJids}
			captionMessage.ExtendedTextMessage = &proto.ExtendedTextMessage{
				Text:        &caption,
				ContextInfo: captionCtx,
			}
		} else {
			// No mentions, send as simple conversation
			captionMessage.Conversation = &caption
		}

		captionID := whatsmeow.GenerateMessageID() // Generate a new ID for the caption
		_, captionErr := b.wc.SendMessage(context.TODO(), groupJID, &captionMessage, whatsmeow.SendRequestExtra{ID: captionID})
		if captionErr != nil {
			b.Log.Errorf("Failed to send caption for audio message: %v", captionErr)
			// Return the original audio message ID but log the caption error
			// Or potentially return the caption error if it's more critical
		}
	}

	// Return the ID of the main audio message
	return audioMsgID, err // Return the result of sending the audio part
}

// Send a message from the bridge to WhatsApp
func (b *Bwhatsapp) Send(msg config.Message) (string, error) {
	groupJID, _ := types.ParseJID(msg.Channel)

	extendedMsgID, _ := b.parseMessageID(msg.ID)
	msg.ID = extendedMsgID.MessageID

	b.Log.Debugf("=> Receiving %#v", msg)

	// Delete message
	if msg.Event == config.EventMsgDelete {
		if msg.ID == "" {
			// No message ID in case action is executed on a message sent before the bridge was started
			// and then the bridge cache doesn't have this message ID mapped
			return "", nil
		}

		_, err := b.wc.RevokeMessage(groupJID, msg.ID)

		return "", err
	}

	// Edit message
	if msg.ID != "" {
		b.Log.Debugf("updating message with id %s", msg.ID)

		if b.GetString("editsuffix") != "" {
			msg.Text += b.GetString("EditSuffix")
		} else {
			msg.Text += " (edited)"
		}
	}

	// Handle Upload a file
	if msg.Extra["file"] != nil {
		fi := msg.Extra["file"][0].(config.FileInfo)
		filetype := mime.TypeByExtension(filepath.Ext(fi.Name))

		b.Log.Debugf("Extra file is %#v", filetype)

		// TODO: add different types
		// TODO: add webp conversion
		switch filetype {
		case "image/jpeg", "image/png", "image/gif":
			return b.PostImageMessage(msg, filetype)
		case "video/mp4", "video/3gpp": // TODO: Check if codecs are supported by WA
			return b.PostVideoMessage(msg, filetype)
		case "audio/ogg":
			return b.PostAudioMessage(msg, "audio/ogg; codecs=opus") // TODO: Detect if it is actually OPUS
		case "audio/aac", "audio/mp4", "audio/amr", "audio/mpeg":
			return b.PostAudioMessage(msg, filetype)
		default:
			return b.PostDocumentMessage(msg, filetype)
		}
	}

	var message proto.Message
	text := msg.Text + msg.Username
	mentionedJids := b.parseMentions(text) // Parse mentions from text

	// Use ExtendedTextMessage if it's a reply OR if there are mentions
	isReply := msg.ParentID != ""
	hasMentions := len(mentionedJids) > 0

	if isReply || hasMentions {
		var ctx *proto.ContextInfo
		var err error
		if isReply {
			ctx, err = b.getNewReplyContext(msg.ParentID)
			if err != nil {
				b.Log.Warnf("Failed to get reply context for ParentID %s: %v. Sending without reply context.", msg.ParentID, err)
				// Fallback: create a new context or proceed without reply context
				ctx = &proto.ContextInfo{} // Create empty context if reply fails but mentions exist
			}
		}
		// Ensure context exists if needed (either from reply or for mentions)
		if ctx == nil {
			ctx = &proto.ContextInfo{}
		}

		// Add mentions if they exist
		if hasMentions {
			ctx.MentionedJID = mentionedJids
		}

		message = proto.Message{
			ExtendedTextMessage: &proto.ExtendedTextMessage{
				Text:        &text,
				ContextInfo: ctx,
			},
		}
	} else {
		// No reply and no mentions, send as simple conversation
		message.Conversation = &text
	}

	return b.sendMessage(msg, &message)
}

func (b *Bwhatsapp) sendMessage(rmsg config.Message, message *proto.Message) (string, error) {
	groupJID, _ := types.ParseJID(rmsg.Channel)
	ID := whatsmeow.GenerateMessageID()

	_, err := b.wc.SendMessage(context.Background(), groupJID, message, whatsmeow.SendRequestExtra{ID: ID})

	return getMessageIdFormat(*b.wc.Store.ID, ID), err
}

// parseMentions finds mentions in the format @<number> and returns a list of corresponding JIDs.
func (b *Bwhatsapp) parseMentions(text string) []string {
	// Regular expression to find @ followed by digits
	re := regexp.MustCompile(`@(\d+)`)
	matches := re.FindAllStringSubmatch(text, -1)

	var mentionedJids []string
	if len(matches) > 0 {
		jidsMap := make(map[string]struct{}) // Use a map to store unique JIDs
		for _, match := range matches {
			if len(match) > 1 {
				// Construct JID: number@s.whatsapp.net
				jid := match[1] + "@s.whatsapp.net"
				// Validate the constructed JID (optional but recommended)
				if _, err := types.ParseJID(jid); err == nil {
					if _, exists := jidsMap[jid]; !exists {
						jidsMap[jid] = struct{}{}
						mentionedJids = append(mentionedJids, jid)
					}
				} else {
					b.Log.Warnf("Invalid JID format created from mention %s: %v", match[0], err)
				}
			}
		}
	}

	if len(mentionedJids) > 0 {
		b.Log.Debugf("Found mentions, JIDs: %v", mentionedJids)
		return mentionedJids
	}
	return nil
}
