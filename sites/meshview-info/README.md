# MeshView informationssider

Lokale kilder til de fire selvstændige sider. Ikke CoreScopes `public/` og ikke automatisk deployet.

| Fil | Offentlig rute |
| --- | --- |
| index.html | /velkommen |
| observer.html | /observer |
| hardware.html | /hardware |
| radio.html | /radio |

## Assets og senere deployment

HTML bruger `/meshview-info-assets/site.css` og observer bruger desuden
`/meshview-info-assets/copy-commands.js`. Fonts hentes relativt til CSS-filen.
En senere, særskilt godkendt deployment skal derfor også kopiere `assets/`
og tilføje en statisk Caddy-route før CoreScope-fallbacken, eksempelvis:

```caddyfile
handle_path /meshview-info-assets/* {
    root * /var/www/meshview-info/assets
    file_server
}
```

Dette er kun dokumentation; produktionskonfigurationen er ikke ændret.
Deploy ikke HTML alene. Test scripts, fonts og CSS med korrekt MIME-type og
HTTP 200 før godkendelse. Kopiér ikke tests eller denne README til webroden.

## Test

Fra repoet, med Playwright tilgængelig:

```sh
node sites/meshview-info/test-copy-commands.js
node sites/meshview-info/test-mobile.cjs
```

Mobiltesten starter/lukker selv en lokal server på en ledig loopback-port,
bruger syntetiske API-data og blokerer eksterne browserrequests. Den tester
Chromium ved 320/375/390/768/1280 px. `SCREENSHOT_DIR` kan sættes for billeder.
Det er ikke en test på fysisk iPhone/Safari eller af produktionens API-data.

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

## Separat fund — ikke rettet her

Forsidens eksisterende `renderNodes`, `renderObservers` og `renderPackets`
interpolerer API-felter direkte i `innerHTML`. Det bør sikkerhedsreviewes og
rettes separat med regressionsprøver for HTML-injektion. Layouttesten bruger
almindelig syntetisk tekst og er ikke en sikkerhedsgodkendelse af disse renderere.
