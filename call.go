// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	waBinary "github.com/go-whatsapp/whatsmeow/binary"
	"github.com/go-whatsapp/whatsmeow/types"
	"github.com/go-whatsapp/whatsmeow/types/events"
)

func (cli *Client) handleCallEvent(node *waBinary.Node) {
	go cli.sendAck(node)

	if len(node.GetChildren()) != 1 {
		cli.dispatchEvent(&events.UnknownCallEvent{Node: node})
		return
	}
	ag := node.AttrGetter()
	child := node.GetChildren()[0]
	cag := child.AttrGetter()
	basicMeta := types.BasicCallMeta{
		From:        ag.JID("from"),
		Timestamp:   ag.UnixTime("t"),
		CallCreator: cag.JID("call-creator"),
		CallID:      cag.String("call-id"),
	}
	switch child.Tag {
	case "offer":
		cli.dispatchEvent(&events.CallOffer{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "offer_notice":
		cli.dispatchEvent(&events.CallOfferNotice{
			BasicCallMeta: basicMeta,
			Media:         cag.String("media"),
			Type:          cag.String("type"),
			Data:          &child,
		})
	case "relaylatency":
		cli.dispatchEvent(&events.CallRelayLatency{
			BasicCallMeta: basicMeta,
			Data:          &child,
		})
	case "accept":
		cli.dispatchEvent(&events.CallAccept{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "preaccept":
		cli.dispatchEvent(&events.CallPreAccept{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "transport":
		cli.dispatchEvent(&events.CallTransport{
			BasicCallMeta: basicMeta,
			CallRemoteMeta: types.CallRemoteMeta{
				RemotePlatform: ag.String("platform"),
				RemoteVersion:  ag.String("version"),
			},
			Data: &child,
		})
	case "terminate":
		cli.dispatchEvent(&events.CallTerminate{
			BasicCallMeta: basicMeta,
			Reason:        cag.String("reason"),
			Data:          &child,
		})
	default:
		cli.dispatchEvent(&events.UnknownCallEvent{Node: node})
	}
}

func (cli *Client) RejectCall(callID string, callFrom types.JID, messageID types.MessageID) error {
	clientID := cli.getOwnJID()
	if clientID.IsEmpty() {
		return ErrNotLoggedIn
	}
	if messageID == "" {
		messageID = cli.GenerateMessageID()
	}
	clientID = clientID.ToNonAD()
	callFrom = callFrom.ToNonAD()

	return cli.sendNode(waBinary.Node{
		Tag: "call",
		Attrs: waBinary.Attrs{
			"id":   messageID,
			"from": clientID,
			"to":   callFrom,
		},
		Content: []waBinary.Node{
			{
				Tag: "reject",
				Attrs: waBinary.Attrs{
					"call-id":      callID,
					"call-creator": callFrom,
					"count":        "0",
				},
				Content: nil,
			},
		},
	})
}
