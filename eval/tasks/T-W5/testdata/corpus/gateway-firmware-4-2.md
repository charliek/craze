URL: https://www.tessaroot.example/changelog/gateway-firmware-4-2
Title: Gateway firmware 4.2: a week of offline buffering - Tessaroot changelog

# Gateway firmware 4.2: a week of offline buffering

Firmware 4.2 for the Tessaroot G3 and G4 gateways rolls out to every greenhouse from
3 March 2026.

- **A week of buffering.** A gateway that loses its uplink now keeps recording for up to
  seven days (it was 36 hours) and uploads everything when it reconnects. Expect the API to
  answer `TSR-7719` for ranges covering the outage while a gateway catches up; the
  troubleshooting page explains how to read those ranges.
- **Faster sensor pairing.** A new sensor pairs in under ten seconds.
- **Clock sync.** Gateways now sync their clocks every hour, so readings' timestamps drift
  by less than a second.

The update installs itself overnight; no action is needed on the gateway.
