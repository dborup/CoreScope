# Screenshots for #235 (48 px touch targets)

Evidence only; not part of any PR diff. Taken with Playwright Chromium against
a local Go server on the CI E2E fixture (`test-fixtures/e2e-fixture.db`, freshened,
seeded and migrated as in `deploy.yml`).

- `before/`: master `aff158c7`
- `after/`: branch `codex/issue-235-touch-targets-48`

Viewports: `m390` = 390x844 touch (isMobile, DPR 2), `t768` = 768x1024 touch,
`d1440` = 1440x900 desktop. Every shot measured `scrollWidth - clientWidth = 0`
(no horizontal page overflow).
