# Your owner account

Sign in with your companion radio, and your **My Mesh** list follows you to every phone and computer. There is no password and no e-mail: your companion is your account.

The site's operator must switch this on (`accounts.enabled`, see [Configuration → Owner accounts](configuration.md#owner-accounts)). If **Account** on the Home page is missing, it is off.

## Signing in

You need your companion and the MeshCore app that controls it.

1. Open **Home** and tap **Sign in with your companion** (or go to `#/account`).
2. **Which companion?** Type its name and pick it from the list, or paste its public key. Only companions can sign in. If yours is not found, send a flood advert from it first so an observer hears it, then search again.
3. **Your code.** The page shows a 6-character code and a suggested name: your current name with the code appended. Tap **Copy name**, set it as your companion's name in the app, then send a **Flood** advert.
   - The byte counter says whether the code fits. A companion that does not share its position has 31 bytes for its name on air; one that shares its position has 23. A code past that point is cut off by the radio and proves nothing.
   - Use **Flood**, not zero-hop: a zero-hop advert only reaches radios right next to you.
4. **Waiting.** The page checks for you. Within seconds of an observer hearing the advert, it signs you in. After two minutes without an answer it suggests sending the advert again, or trying closer to a repeater. The code works for 15 minutes.
5. **Done.** The page says **Signed in as** your companion's name and the first 8 characters of its key. Tap **This is me**, or **Not me – cancel** if it is the wrong companion.

Then give your companion its old name back. The name with the code is public for as long as it is set.

### Why this is safe

- The advert is signed by your companion's private key, and the signature covers the name. Only your companion (or someone holding it) can send the code.
- The code is new for every sign-in, works once and is tied to the companion you chose. Someone who sees it on the air cannot use it: the session goes only to the browser that asked for the code.
- A repeater, room server or sensor can never sign in, whatever its name says.

## My nodes

Signed in, **My Mesh** on Home shows your account's list. Add a node with the search on Home, remove it with ✕ on its card or on the account page. The change reaches your other devices the next time they load a page.

- **The nodes you saved before signing in** stay in this browser. The first time you sign in, the account page offers to add them: **Add all** copies them to your account, **Not now** asks no more in this browser. Nothing is deleted from the browser either way, and you see that list again after signing out.
- An account holds up to 200 nodes (the operator may set another limit). Home shows 24 cards at a time; **Show more** adds the next 24.
- My nodes is a watch list: adding a node proves nothing about who owns it, and it does not let you change its data. Setting a node's antenna height or station details still works per node: see [Setting your node's antenna height](antenna-height-claims.md).

## Privacy

- Nobody else can see that your companion has an account, or which nodes are on your list. The companion's key is never shown as part of the account; the account page shows only its first 8 characters, to you.
- The optional **display name** on the account page is not shown publicly.
- The site stores your account id (random, not derived from the key), the companion's public key, the display name, and when you first and last signed in. It stores no IP address and no browser details.

## Signing out and deleting

- **Sign out** ends the session in this browser.
- **Sign out everywhere** ends every session of the account, on every device, within a few seconds. Use it if you lost a phone or signed in on a shared computer.
- **Delete account** deletes the account and its list. Station data you set for your nodes is not touched (it is not part of the account).
- An account nobody signs in to for 24 months is deleted automatically.
- A session lasts 30 days and is renewed while you use it, but you must sign in again with your companion at least every 90 days.

Deletion is logical: as with every other deletion on this site, an earlier database backup can still hold the deleted rows until it is replaced.
