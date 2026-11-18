# Setting your node's antenna height

Path profiles need the height of each antenna above the ground. A mesh advert carries no height, so a profile normally uses an estimate: the roof or treetop around a repeater or room server from the surface model plus 2 m, or a default for the node type. A mast does not show in the surface model.

If you own a node, you can give the site its real antenna height. You prove that you control the node by putting a short code into the node's name and sending an advert. The advert is signed with the node's key, so only someone with the node, or with its admin login, can send it.

This page is for node owners. Operators: see [Configuration → Owner antenna heights](configuration.md#owner-antenna-heights).

## Before you start

- The site must have owner claims switched on. If the node page has no **Antenna height (owner)** section, they are off on this site.
- The page offers claims for **repeaters** and **room servers**.
- The node must send its position in its adverts. Without a position, a height cannot be claimed.
- You need access to the node's command line: over USB, or remotely as admin from the MeshCore app.

## Steps

1. Open the node's page and go to **Antenna height (owner)**. Use the link `#/nodes/<public key>?section=node-antenna` to jump there.
2. The section shows the **current height** and where it comes from (see [Sources](#where-the-current-height-comes-from)).
3. Choose **Set the height**, enter the antenna height above the ground in metres (0–100 m, in steps of 0.1 m, for example `18.5`), and press **Get code**.
   - To remove a height you set before, choose **Remove my height** instead.
4. The page shows a 6-character code, such as `7KX9QM`, and a suggested name: your current name with the code added at the end.
5. Set the node's name to the suggested name and send an advert. Use the CLI or the app, as described below.
6. Keep the page open. It checks the status by itself and tells you when the advert has arrived.
7. When the page says **Proven**, give the node its usual name back and send another advert. The code has done its job.

The code works once, only for this node, and expires after 24 hours. It is not a secret: the advert's signature does the security work. The code is never put in the page's link.

### Repeater or room server: CLI over USB

Connect to the node's serial console and run:

```
set name Roof Repeater 7KX9QM
advert
```

The page builds the exact `set name` line for you, with a Copy button.

### Repeater or room server: remotely from the MeshCore app

1. In the MeshCore app, log in to the repeater or room server as admin.
2. Open its command line.
3. Run the same two commands: `set name …` with the suggested name, then `advert`.

The commands work the same remotely as over USB.

## The 23-byte name rule

An advert with a position has room for **23 bytes** of name. The node sends only the first 23 bytes and cuts the rest, so a code past byte 23 never goes on air and proves nothing.

- Letters A–Z, digits and spaces take 1 byte each. Letters like `æ`, `ø` and `å` take 2 bytes. An emoji usually takes 4.
- The code must be a separate word: a space before it, and nothing glued to it.
- Put the code at the end, after a shortened name if needed. The page's name field counts the bytes as you type and says whether the code fits.
- The node's CLI refuses names with `[ ] \ : , ? *`.

The suggested name already follows these rules.

## What the statuses mean

| Status | Meaning |
|---|---|
| Registering your request | The site has the request and is setting it up. |
| Waiting for an advert | Send an advert with the code in the name. |
| Proven | The advert arrived with the code. An administrator reviews the height next, unless the site approves proven heights by itself. You can rename the node now. |
| Approved | Path profiles use your height now. It can take up to a minute to show everywhere. |
| Rejected | An administrator refused the claim. Ask the site's operator why. |
| Revoked | An administrator withdrew an approved height. |
| Expired | No advert with the code arrived within 24 hours. Ask for a new code. |
| Superseded | A newer claim for the node replaced this one. |
| Stopped checking automatically | The page stopped checking after a while. The claim keeps waiting; press **Check again** after sending the advert. |

An administrator may look closer at a height that is very far above the surface estimate, or below the surface.

## Where the current height comes from

The section shows the height a path profile uses for the node, with a badge:

| Badge | Source |
|---|---|
| **operator** | The site's operator set a height for this node, or for its role. It wins over yours. |
| **owner** | Your approved height. |
| **estimate** | The surface model: roof or treetop plus 2 m, never below the role default. |
| **default** | A default for the node type. |

A height that a viewer types into a path profile, or that comes from a shared link, wins for that viewer only.

If the node moves more than 50 m (the site may set another distance) from where you proved the height, the site stops using your height until you claim it again.

Viewers can turn **Prefer owner heights** off in Customize → Display → Terrain Profile. Their own default heights then replace owner heights in their profiles. This changes nothing for anyone else.

## How to clear your height

1. Choose **Remove my height** in the section and press **Get code**.
2. Put the code into the node's name and send an advert, as above.

When that advert arrives, your height is removed at once, without review. Profiles then fall back to the estimate or the default. Rename the node back afterwards.

## Privacy

The site stores the node's public key, the height, the code, and the time and position of the proving advert. Finished claims are deleted after a retention period set by the operator; the approved one is kept while it is in force. Nothing about you as a person is asked for or stored.
