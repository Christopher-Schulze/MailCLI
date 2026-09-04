package mailstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maximumMailboxCacheBytes = 2 * 1024 * 1024

func (s *Store) loadMailboxCache(ctx context.Context, accountID string) (mailboxCache, error) {
	if err := ctx.Err(); err != nil {
		return mailboxCache{}, err
	}
	path := filepath.Join(s.versionRoot, accountID, ".mboxCache.plist")
	file, info, err := openRegularPath(s.versionDirectory, s.versionRoot, path)
	if err != nil {
		return mailboxCache{}, fmt.Errorf("open mailbox cache: %w", err)
	}
	if info.Size() < 1 || info.Size() > maximumMailboxCacheBytes {
		resultErr := error(fmt.Errorf("mailbox cache is not a bounded regular file"))
		joinCloseError(&resultErr, file, "mailbox cache")
		return mailboxCache{}, resultErr
	}
	return readMailboxCache(ctx, file, info)
}

func readMailboxCache(
	ctx context.Context,
	file *os.File,
	expected os.FileInfo,
) (result mailboxCache, resultErr error) {
	defer joinCloseError(&resultErr, file, "mailbox cache")
	opened, err := file.Stat()
	if err != nil {
		return mailboxCache{}, operationError("invalid_mailbox_cache", fmt.Sprintf("inspect opened mailbox cache: %v", err))
	}
	if !os.SameFile(expected, opened) || expected.Size() != opened.Size() {
		return mailboxCache{}, operationError("invalid_mailbox_cache", "mailbox cache changed while opening")
	}
	cache, err := parseMailboxCacheXML(io.LimitReader(file, maximumMailboxCacheBytes+1))
	if err != nil {
		return mailboxCache{}, operationError("invalid_mailbox_cache", err.Error())
	}
	if err := ctx.Err(); err != nil {
		return mailboxCache{}, err
	}
	return cache, nil
}

func parseMailboxCacheXML(reader io.Reader) (mailboxCache, error) {
	decoder := xml.NewDecoder(reader)
	root, err := nextPlistStart(decoder)
	if err != nil || root.Name.Local != "plist" {
		return mailboxCache{}, fmt.Errorf("parse mailbox cache: expected plist root")
	}
	dictionary, err := nextPlistStart(decoder)
	if err != nil || dictionary.Name.Local != "dict" {
		return mailboxCache{}, fmt.Errorf("parse mailbox cache: expected root dictionary")
	}
	cache, err := parseMailboxCacheRoot(decoder)
	if err != nil {
		return mailboxCache{}, fmt.Errorf("parse mailbox cache: %w", err)
	}
	if len(cache.Mailboxes) == 0 {
		return mailboxCache{}, fmt.Errorf("mailbox cache contains no mailbox catalog")
	}
	return cache, nil
}

func parseMailboxCacheRoot(decoder *xml.Decoder) (mailboxCache, error) {
	var cache mailboxCache
	for {
		key, value, done, err := nextPlistPair(decoder)
		if err != nil || done {
			return cache, err
		}
		if key != "mboxes" {
			if err := decoder.Skip(); err != nil {
				return mailboxCache{}, err
			}
			continue
		}
		if value.Name.Local != "dict" || cache.Mailboxes != nil {
			return mailboxCache{}, fmt.Errorf("mboxes must be one dictionary")
		}
		cache.Mailboxes, err = parseMailboxCacheNodes(decoder)
		if err != nil {
			return mailboxCache{}, err
		}
	}
}

func parseMailboxCacheNodes(decoder *xml.Decoder) (map[string]mailboxCacheNode, error) {
	nodes := make(map[string]mailboxCacheNode)
	for {
		key, value, done, err := nextPlistPair(decoder)
		if err != nil || done {
			return nodes, err
		}
		if value.Name.Local != "dict" {
			return nil, fmt.Errorf("mailbox %q must be a dictionary", key)
		}
		node, err := parseMailboxCacheNode(decoder)
		if err != nil {
			return nil, err
		}
		if _, exists := nodes[key]; exists {
			return nil, fmt.Errorf("duplicate mailbox key %q", key)
		}
		nodes[key] = node
	}
}

func parseMailboxCacheNode(decoder *xml.Decoder) (mailboxCacheNode, error) {
	var node mailboxCacheNode
	for {
		key, value, done, err := nextPlistPair(decoder)
		if err != nil || done {
			return node, err
		}
		switch key {
		case "MailboxPathComponent":
			node.PathComponent, err = decodePlistString(decoder, value)
		case "MailboxUnreadCount":
			node.UnreadCount, err = decodePlistInteger(decoder, value)
		case "IMAPMailboxAttributes":
			node.Attributes, err = decodePlistInteger(decoder, value)
		case "IMAPMailboxChildren":
			if value.Name.Local != "dict" {
				return mailboxCacheNode{}, fmt.Errorf("mailbox children must be a dictionary")
			}
			node.Children, err = parseMailboxCacheNodes(decoder)
		default:
			err = decoder.Skip()
		}
		if err != nil {
			return mailboxCacheNode{}, err
		}
	}
}

func nextPlistPair(decoder *xml.Decoder) (string, xml.StartElement, bool, error) {
	start, done, err := nextPlistElement(decoder)
	if err != nil || done {
		return "", xml.StartElement{}, done, err
	}
	if start.Name.Local != "key" {
		return "", xml.StartElement{}, false, fmt.Errorf("expected dictionary key, got %s", start.Name.Local)
	}
	key, err := decodePlistString(decoder, start)
	if err != nil {
		return "", xml.StartElement{}, false, err
	}
	value, done, err := nextPlistElement(decoder)
	if err != nil || done {
		return "", xml.StartElement{}, false, fmt.Errorf("dictionary key %q has no value", key)
	}
	return key, value, false, nil
}

func nextPlistStart(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		start, done, err := nextPlistElement(decoder)
		if err != nil {
			return xml.StartElement{}, err
		}
		if !done {
			return start, nil
		}
	}
}

func nextPlistElement(decoder *xml.Decoder) (xml.StartElement, bool, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, false, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			return value, false, nil
		case xml.EndElement:
			return xml.StartElement{}, true, nil
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return xml.StartElement{}, false, fmt.Errorf("unexpected plist text")
			}
		}
	}
}

func decodePlistString(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	if start.Name.Local != "key" && start.Name.Local != "string" {
		return "", fmt.Errorf("expected plist string, got %s", start.Name.Local)
	}
	var value string
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return "", err
	}
	return value, nil
}

func decodePlistInteger(decoder *xml.Decoder, start xml.StartElement) (int, error) {
	if start.Name.Local != "integer" {
		return 0, fmt.Errorf("expected plist integer, got %s", start.Name.Local)
	}
	var value string
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return 0, err
	}
	integer, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("parse plist integer: %w", err)
	}
	return integer, nil
}
