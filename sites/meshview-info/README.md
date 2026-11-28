# MeshView informationssider

Lokale kilder til de fire selvstændige sider. Ikke CoreScopes `public/` og ikke automatisk deployet.

| Fil | Offentlig rute |
| --- | --- |
| index.html | /velkommen |
| observer.html | /observer |
| hardware.html | /hardware |
| radio.html | /radio |
| guider.html | /guider |
| faq.html | /faq |

## Assets og deployment

HTML bruger `/meshview-info-assets/site.css`, og observer bruger desuden
`/meshview-info-assets/copy-commands.js`, og guider bruger
`/meshview-info-assets/guider.js` og miniaturerne i `assets/guider/`. Fonts hentes relativt til CSS-filen.

Siderne serveres som statiske filer på de offentlige ruter ovenfor
(skrivebeskyttet, uden om CoreScopes `public/`). De deployes ikke automatisk:
en ændring kopieres til serveren manuelt og kun efter ejerens godkendelse.
Deploy aldrig HTML alene; `assets/` skal med. Kopiér ikke tests eller denne
README til webroden. Tjek scripts, fonts og CSS med korrekt MIME-type og
HTTP 200 efter en deploy.

Denne mappe er kilden. Siderne på serveren svarede byte for byte til den
2026-11-28 (bortset fra det script, CDN'et selv indsætter).

## Test

Fra repoet, med Playwright tilgængelig:

```sh
node sites/meshview-info/test-copy-commands.js
node sites/meshview-info/test-xss.cjs
node sites/meshview-info/test-mobile.cjs
node sites/meshview-info/test-guider.cjs
node sites/meshview-info/test-faq.cjs
```

Mobiltesten starter/lukker selv en lokal server på en ledig loopback-port,
bruger syntetiske API-data og blokerer eksterne browserrequests. Den tester
Chromium ved 320/375/390/768/1280 px. `SCREENSHOT_DIR` kan sættes for billeder.
Det er ikke en test på fysisk iPhone/Safari eller af produktionens API-data.

## Videoguider (/guider)

Hver guide er et kort i `guider.html` med `data-youtube=""`. Når videoen er lagt
på YouTube, sættes dens 11-tegns id (fx `data-youtube="dQw4w9WgXcQ"`). Et kort
uden gyldigt id vises som "Video på vej" og kan ikke afspilles.

Afspilning sker først ved klik: `assets/guider.js` skifter miniaturen ud med en
`youtube-nocookie.com`-afspiller. Før klikket hentes intet fra YouTube;
miniaturerne ligger lokalt i `assets/guider/` (640×360 jpg, taget fra videoen).
Ny guide: læg miniaturen i `assets/guider/`, kopiér et kort, og link til den
matchende side i `docs/user-guide/`.

## FAQ (/faq)

Hvert spørgsmål er et `<details class="faq-item" id="…">` i en gruppe. `id` er
det dybe link (fx `/faq#skjul-node`), som åbner svaret; ændr ikke et id, der
allerede er linket til. Svarene om data og privatliv skal passe med serverens
indstillinger (`retention`, `hiddenNamePrefixes`, kanalnøgler); ret dem, når
indstillingerne ændres.

## Fonts

Uændrede TTF-filer hentet 2026-09-16 fra google/fonts, med OFL-licenser:

- https://github.com/google/fonts/tree/main/ofl/syne
- https://github.com/google/fonts/tree/main/ofl/sharetechmono

Begge serveres lokalt med `font-display: swap`; ingen Google Fonts requests.

## Indholdskontrol 2026-09-16

- [MeshCore CLI](https://github.com/meshcore-dev/MeshCore/blob/main/docs/cli_commands.md)
  og [CommonCLI.cpp](https://github.com/meshcore-dev/MeshCore/blob/main/src/helpers/CommonCLI.cpp):
  kontrolleret hash-mode 1 = 2 byte, loop-detect, delay-parametre og advert-intervaller.
  Lokale værdier er ikke firmwaredefaults. Scopeteksten er rettet: regioner
  kan begrænse flood-videresendelse; wildcard gælder unscoped trafik.
- Frekvens 869.618, BW 62.5, SF8, CR8 og den lokale delay-tabel er bevaret,
  men er ikke uafhængigt bekræftet som aktuelle for hele Danmark.
  Formuleringer om landsdækkende garanti og obligatorisk uges indlæring er fjernet.
- [T096](https://heltec.org/project/t096/): nRF52840/SX1262/GNSS bekræftet.
  Den ubekræftede specifikke 0.96-tommer-skærmpåstand er fjernet.
- [Wireless Tracker V2](https://heltec.org/project/wireless-tracker-v2/): ESP32-S3/SX1262/GNSS bekræftet.
- [L1](https://wiki.seeedstudio.com/wio_tracker_l1_node/) og
  [P1-Pro](https://www.seeedstudio.com/SenseCAP-Solar-Node-P1-Pro-for-Meshtastic-LoRa-p-6412.html):
  nRF52840/SX1262 bekræftet. Produktlinks går nu til producentens MeshCore-varianter.
- [Station-salgssiden](https://store.bqvoy.com/product/mesh-device-station-edition/)
  er omlagt fra G2 til G3. Modellerne må ikke forveksles; guiden har nu en note.
- Eksterne hardware/radio-links returnerede HTTP 200, bortset fra
  meshguide.dk som returnerede 455 til automatisk curl. Linket er bevaret;
  adgang i almindelig browser skal bekræftes før publicering.
- Ingen fysisk hardwaretest, købstest eller fuld firmwarekompatibilitetstest
  af hver model. Uunderbyggede generelle batteri-/test-/popularitetspåstande er nedtonet.

## HTML-injektion i API-visning

Den oprindelige forside interpolerede API-felter direkte i `innerHTML`.
En isoleret Chromium-regression med syntetiske felter reproducerede
HTML-injektion og eksekvering af en ufarlig lokal testmarkør. Testen har
blokeret netværk og blev ikke kørt på produktion. Ingest/API-kædens mulighed
for at levere et angriberstyret payload er ikke undersøgt, og der er ingen
konklusion om faktisk misbrug.

Regressionen dækker navne, fallbackfelter, role-attribut, numeriske felter,
bevarelse af bogstavelig tekst og normal formatering i de tre renderere.
Rettelsen HTML-escaper tekst, begrænser rolleklasser til kendte værdier og
normaliserer numeriske felter før interpolation. De eksisterende API-kald
og maksimumantal viste rækker er uændrede. Dette er en lokal rettelse;
den er ikke publiceret på prod.
