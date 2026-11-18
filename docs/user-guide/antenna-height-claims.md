# Setting your node's antenna height

Path profiles need the height of each antenna above the ground. A mesh advert carries no height, so a profile normally uses an estimate: the roof or treetop around a repeater or room server from the surface model plus 2 m, or a default for the node type. A mast does not show in the surface model.

If you own a node, you can give the site its real antenna height. You prove that you control the node by putting a short code into the node's name and sending an advert. The advert is signed with the node's key, so only someone with the node, or with its admin login, can send it.

This page is for node owners. Operators: see [Configuration → Owner antenna heights](configuration.md#owner-antenna-heights).

Besides the height, a site can let you describe the station: antenna type, gain, power source and installation, plus optional contacts. See [Station details](#station-details).

## Before you start

- The site must have owner claims switched on. If the node page has no **Antenna height (owner)** or **Antenna and station (owner)** section, they are off on this site. The second title means station details are on too.
- The page offers claims for **repeaters** and **room servers**.
- The node must send its position in its adverts. Without a position, a height cannot be claimed.
- You need access to the node's command line: over USB, or remotely as admin from the MeshCore app.

## Steps

1. Open the node's page and go to **Antenna height (owner)**. Use the link `#/nodes/<public key>?section=node-antenna` to jump there.
2. The section shows **this site's height** for the node and where it comes from (see [Sources](#where-the-current-height-comes-from)).
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
| Error (the page shows the reason) | The site could not carry out the request, for example because the node already has too many claims waiting, too many claims are waiting on the whole site, or the claim could not be saved. Nothing changed. Ask for a new code later, or ask the site's operator. |
| Stopped checking automatically | The page stopped checking after a while. The claim keeps waiting; press **Check again** after sending the advert. |

An administrator may look closer at a height that is very far above the surface estimate, or below the surface.

## Where the current height comes from

The section shows the height this site uses for the node in path profiles, with a badge. The first of these that exists is used:

| Badge | Source |
|---|---|
| **operator** | The site's operator set a height for this node. It wins over yours. |
| **owner** | Your approved height. It wins over everything below. |
| **estimate** | The surface model: roof or treetop plus 2 m, used only where it is above the role height below. |
| **operator** | The site's operator set a height for this node type (role). |
| **default** | The built-in default for the node type. |

A height that a viewer types into a path profile, or that comes from a shared link, wins for that viewer only.

If the node moves more than 50 m (the site may set another distance) from where you proved the height, the site stops using your height until you claim it again.

Viewers can turn **Prefer owner heights** off in Customize → Display → Terrain Profile. Their own role heights there (the built-in defaults unless they changed them) then replace owner heights in their profiles. Their role heights can also replace a **default** height. This changes nothing for anyone else, and the section keeps showing the site's height.

## How to clear your height

1. Choose **Remove my height** in the section and press **Get code**.
2. Put the code into the node's name and send an advert, as above.

When that advert arrives, your height is removed at once, without review. Profiles then fall back to the estimate or the default. Rename the node back afterwards.

## Station details

When the site has station details switched on, the section is called **Antenna and station (owner)** and the form has two groups: **Antenna height** and **Station details**. Each group has three choices:

| Choice | What it does |
|---|---|
| **Keep** | Leaves that part as it is. This is the default. |
| **Set** | Replaces that part with what you enter. |
| **Remove** | Removes that part. |

You can change the height, the details or both in one claim. One claim has one code, one advert proves it, and an administrator approves or rejects the whole claim. If one part is wrong, the claim is rejected and you submit a new one.

A claim replaces only the parts it carries. A claim that sets the height leaves your approved details in force, and the other way round.

The details are:

| Field | Values |
|---|---|
| Antenna type | omni, yagi, panel or other |
| Antenna gain | −5 to 20 dBi, in steps of 0.1. A gain outside 0–15 dBi gets a closer look from an administrator. |
| Power | any of mains, solar and battery |
| Installation | roof, mast, indoor or portable |
| Public contact | optional; see below |
| Administrator contact | optional; see below |

At least one field must be filled in when you set the details. **Setting the details replaces all of them.** Nothing is merged, so if you change the gain, type your contacts again. The public values are pre-filled for you; the administrator contact never is, because no page can read it.

Station details are for repeaters and room servers, like heights. A claim that sets details is never approved automatically: an administrator always reads it, because the public contact is free text that would otherwise go public unchecked. A claim that only removes things takes effect at once after its advert.

### The two contacts

| | Public contact | Administrator contact |
|---|---|---|
| Who sees it | Everyone, on the node's page and in the site's station API, once the claim is approved | Only the site's administrators, in the admin queue, once the claim is **proven** (not while it is still waiting for the advert) |
| Meant for | A callsign, handle or name you want known | The site's operator reaching you about this site |
| Required | No. It is empty unless you fill it in, and the form says plainly that it is public. | No |

Both follow the same rules: 1–32 characters (at most 64 bytes) made of letters, digits, single spaces and `. _ - @ # + /`, with no space at the start or end. There is no `:`, so a link cannot be entered, and no control, invisible or direction-changing characters. A contact that breaks the rules is refused, never changed for you.

Neither contact is put in a node card, a link preview, a share image or an export. The public contact shows only on the node page and in the station API. The administrator contact is in no public response at all.

### When the details are deleted

Both contacts live exactly as long as the details they belong to. They are deleted, and the public contact stops being shown, when:

- **you remove the details** (a proven clear takes effect at once, without review);
- **an administrator rejects** the claim;
- **an administrator revokes** the approved claim;
- **the claim expires**, because no advert with the code arrived within 24 hours;
- **a newer approved claim replaces the details**, including a claim that only removes them;
- **retention** deletes the finished claim, after the operator's retention period.

Details that nothing replaced stay in force with no time limit. If the node moves more than 50 m (the operator may set another distance) from where you proved them, the site stops showing them until you claim them again. A move deletes nothing, and the administrator can still see them.

If the site switches station details off, you can no longer set details, but you can still remove ones you set earlier while claims are open.

### What stays behind

Deletion is **logical**, not a wipe of the disk:

- **Backups.** Administrators can download a copy of the database, and operators back it up. A copy made while a contact exists contains it, and the operator's backup retention decides how long that copy lives. Ask the operator if a contact must be gone from their backups too.
- **Free pages.** SQLite does not overwrite deleted data at once. Deleted text can stay in unused parts of the database file and its write-ahead log until the space is reused or the file is rebuilt. The site does not use SQLite's secure delete.
- **The request queue.** Your request waits in a file next to the database until the site applies it, normally within seconds, and at most 24 hours if the site's ingestor is down. That file holds both contacts and can be read by the site's service user only. It is removed when the request is applied.

This is the operator's own storage. Other visitors never see it.

## Privacy

The site stores the node's public key, the height, the station details you set, the code, and the time and position of the proving advert. The public contact is shown to everyone and the administrator contact only to administrators. Finished claims are deleted after a retention period set by the operator; the approved one is kept while it is in force. Nothing about you as a person is asked for, and both contacts are optional.
