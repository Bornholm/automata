package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// emailSummary is what the reading tools return about a message.
type emailSummary struct {
	UID     uint32
	From    string
	Subject string
	Date    time.Time
}

// dialIMAP opens an authenticated session on the INBOX.
func dialIMAP(cfg memberConfig, password string) (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, cfg.IMAPPort)

	var client *imapclient.Client
	var err error
	// beta.6 : DialInsecure panique sur des options nil.
	opts := &imapclient.Options{}
	if cfg.IMAPInsecure {
		client, err = imapclient.DialInsecure(addr, opts)
	} else {
		client, err = imapclient.DialTLS(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("IMAP connection failed")
	}

	if err := client.Login(cfg.Username, password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("IMAP authentication refused")
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("INBOX unavailable")
	}

	return client, nil
}

// listRecent returns the newest count messages, newest first.
func listRecent(client *imapclient.Client, count int) ([]emailSummary, error) {
	uids, err := searchAll(client)
	if err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, nil
	}

	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > count {
		uids = uids[:count]
	}

	return fetchSummaries(client, uids)
}

// searchAll returns every UID of the selected mailbox.
func searchAll(client *imapclient.Client) ([]uint32, error) {
	data, err := client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search failed")
	}

	var uids []uint32
	for _, uid := range data.AllUIDs() {
		uids = append(uids, uint32(uid))
	}
	return uids, nil
}

// searchSince returns UIDs strictly greater than lastUID.
func searchSince(client *imapclient.Client, lastUID uint32) ([]uint32, error) {
	uids, err := searchAll(client)
	if err != nil {
		return nil, err
	}
	var newer []uint32
	for _, uid := range uids {
		if uid > lastUID {
			newer = append(newer, uid)
		}
	}
	sort.Slice(newer, func(i, j int) bool { return newer[i] < newer[j] })
	return newer, nil
}

// fetchSummaries fetches the envelope of each UID.
func fetchSummaries(client *imapclient.Client, uids []uint32) ([]emailSummary, error) {
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}

	messages, err := client.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch failed")
	}

	var summaries []emailSummary
	for _, msg := range messages {
		summary := emailSummary{UID: uint32(msg.UID)}
		if msg.Envelope != nil {
			summary.Subject = msg.Envelope.Subject
			summary.Date = msg.Envelope.Date
			if len(msg.Envelope.From) > 0 {
				summary.From = msg.Envelope.From[0].Addr()
			}
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].UID > summaries[j].UID })
	return summaries, nil
}

// emailContent is the full message the read tool returns.
type emailContent struct {
	emailSummary
	To        []string
	MessageID string
	Body      string
}

// readEmail fetches one message: envelope + text body.
func readEmail(client *imapclient.Client, uid uint32) (emailContent, error) {
	var set imap.UIDSet
	set.AddNum(imap.UID(uid))

	bodySection := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText}
	messages, err := client.Fetch(set, &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}).Collect()
	if err != nil || len(messages) == 0 {
		return emailContent{}, fmt.Errorf("message %d not found", uid)
	}

	msg := messages[0]
	content := emailContent{emailSummary: emailSummary{UID: uint32(msg.UID)}}
	if msg.Envelope != nil {
		content.Subject = msg.Envelope.Subject
		content.Date = msg.Envelope.Date
		content.MessageID = msg.Envelope.MessageID
		if len(msg.Envelope.From) > 0 {
			content.From = msg.Envelope.From[0].Addr()
		}
		for _, to := range msg.Envelope.To {
			content.To = append(content.To, to.Addr())
		}
	}
	for _, buf := range msg.BodySection {
		content.Body = strings.TrimSpace(string(buf.Bytes))
		break
	}

	return content, nil
}

// searchText searches the mailbox body/subject for the query.
func searchText(client *imapclient.Client, query string) ([]emailSummary, error) {
	data, err := client.UIDSearch(&imap.SearchCriteria{Text: []string{query}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search failed")
	}

	var uids []uint32
	for _, uid := range data.AllUIDs() {
		uids = append(uids, uint32(uid))
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > 10 {
		uids = uids[:10]
	}
	return fetchSummaries(client, uids)
}
