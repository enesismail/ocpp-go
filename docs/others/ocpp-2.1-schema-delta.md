# What actually changed between the OCPP 2.0.1 and OCPP 2.1 JSON schemas

*Ismail Arslan ([arslan.co](https://arslan.co)) — 2026-08-09*

*This note compares the Part 3 JSON schemas of **OCPP 2.0.1 Edition 4** and **OCPP 2.1 Edition 2**, the current published editions as of the date above. It is a snapshot of those two artifacts; later editions may move either side.*

## TL;DR

At the message-envelope level, OCPP 2.1 is a strictly additive superset of OCPP 2.0.1: every one of the 64 message pairs the two versions share has identical top-level `required` arrays, property sets, and types, verified across all 128 common schema files. Twenty-seven wholly new message types were added; none were removed. But drop one level down, into the shared type definitions those messages reference (`IdTokenType`, `SignedMeterValueType`, `ChargingSchedulePeriodType`, and friends), and the additive story breaks in a finite, fully enumerable set of ways — including exactly one new **required** field that makes an old 2.0.1 payload schema-invalid under 2.1. This note lists every one of those exceptions, with the JSON to back each one up.

## Method and provenance

The comparison is between two official Open Charge Alliance downloads, both fetched from `openchargealliance.org/my-oca/ocpp/`:

- **OCPP 2.0.1 Edition 4 (all files)** — the Part 3 JSON schema bundle inside it is dated 2020-03-31, unchanged since the original 2.0.1 release. This is not an inference from a file timestamp: OCA's own bundle changelog excludes the schemas from every edition bump ("All documents except the schemas were updated to the Edition 3/4 version"), and OCA's errata documentation states as standing policy that "the errata do not affect any schemas of OCPP messages" — a sentence that appears verbatim in both the original 2021 errata announcement and the current Errata 2026-06 document, whose Part 3 chapter reads "Currently no new errata for OCPP 2.0.1 part 3". Only the surrounding prose parts have been re-edited through Edition 4.
- **OCPP 2.1 Edition 2 (all files)** — the Part 3 JSON schema bundle inside it is dated 2025-01-23.

Both "all files" ZIPs contain a `Part 3 JSON schemas` sub-ZIP; unzipping it gives a flat directory of `*.json` files, one per message (`FooRequest.json`, `FooResponse.json`). Every quantitative claim in this note comes from diffing those two directories directly with a script, not from a third party's copy of the schemas.

"Additive" is used here in a precise sense, checked programmatically for every file:

- **Top-level**: the schema's own `required` array, its `properties` key set, and its `type` are compared. Removing a property, adding a field to `required`, or changing `type` would each count as non-additive. Adding a new *optional* property does not.
- **Nested**: the same three checks (`required`, `properties`, `type`) are applied to every named object under each file's `definitions` block, since OCPP's Part 3 schemas are self-contained — every file inlines its own copy of every shared type it uses, rather than referencing a separate common schema. On top of the three structural checks, every same-named definition's `enum` value set and every same-named property's `maxLength` are compared, and a definition present on only one side is reported, never skipped.

### Reproducing this comparison

Every number in this note is reproduced by one self-contained script — Python 3, standard library only, published alongside this note as `ocpp-schema-delta.py` — run over the two Part 3 schema directories:

```
python3 ocpp-schema-delta.py <2.0.1-part3-dir> <2.1-part3-dir>
```

It prints the file inventory and Request/Response pairing, the top-level and nested comparison described above, every difference it finds grouped by type definition with the affected files, and a content digest of each directory. The directories this note was produced from, identified by content rather than by name:

| Directory | Documents | SHA-256 over the directory's file digests |
|---|---|---|
| OCPP 2.0.1 Edition 4, `Part 3 JSON schemas` | 128 | `629b20e435babe7338ea16795625a330cace4538e97b0ca4f2d85fde1d723f67` |
| OCPP 2.1 Edition 2, `Part 3 JSON schemas` | 181 | `213e47614976f7a5419abe69b922d0420a5ed820a3042096dcbe09b66df44202` |

Each digest is taken over the directory's `sha256  filename` lines, sorted, so it moves if any document changes, arrives, leaves or is renamed. A reader who extracts the same bundles and computes the same digests is comparing exactly what this note compared.

## Headline numbers

- OCPP 2.0.1: **128 schema files** = 64 message names × {Request, Response}.
- OCPP 2.1: **181 schema files** across **91 message names** — 90 names paired as Request/Response (180 files), and one unpaired: `NotifyPeriodicEventStream.json`, a single schema with no Request/Response suffix.
- The 91 names reconcile as **64 shared with 2.0.1 + 27 new**; the unpaired file is one of the 27. All 64 OCPP 2.0.1 message names exist unchanged in OCPP 2.1. **Zero messages were removed.**
- Across all 128 shared schema files, there are **zero** top-level `required` differences, **zero** top-level property removals, and **zero** top-level `type` changes. This is the strongest evidence for the additive claim, and it holds exhaustively, not just for a sample.
- The nested layer — where every exception below lives — was walked with the checks above: **143 distinct named definitions**, inlined per file as **470 definition blocks** across the 128 shared 2.0.1 files (each Part 3 schema embeds its own copy of every shared type it uses), each compared against its counterpart in the same-named 2.1 file, with anything new on the 2.1 side of those files surfacing as an addition.

The one unpaired file is not an accident of naming. OCPP 2.1's Part 4 (WebSocket/JSON-RPC framing) adds two frame types to 2.0.1's `CALL`/`CALLRESULT`/`CALLERROR`: `CALLRESULTERROR` (an error response to a CALLRESULT) and `SEND`, an unconfirmed, fire-and-forget message used for high-frequency data such as periodic telemetry. Part 4 gives `NotifyPeriodicEventStream` as its own worked example of a SEND payload — a single schema instead of a Request/Response pair is exactly what a SEND-framed message should look like. Real design feature, not a typo, but it will break any tooling that assumes strict Request/Response pairing per action name.

## The 27 new messages, by theme

- **DER control** (6 messages): device-level control of Distributed Energy Resource behavior — reading, writing, clearing, and reporting inverter-level control curves and set-points, plus DER-originated alarm/start-stop notifications.
- **V2X / bidirectional and grid signals** (4 messages): frequency-regulation-style grid signals and dynamic charging/discharging schedule exchange for vehicle-to-grid and other bidirectional power flows.
- **Battery swapping** (2 messages): requesting and executing a battery-swap operation, for charging models built around exchanging a battery rather than charging one in place.
- **Periodic event streams** (5 messages): opening, adjusting, closing, and querying a subscription-style stream of periodic telemetry, plus the one-way `SEND`-framed data message itself.
- **Tariffs / cost / payment** (7 messages): setting and clearing tariffs at runtime, changing a tariff mid-transaction, settlement and web-payment notifications, and VAT number validation.
- **Priority charging** (2 messages): notifying and invoking a priority-charging mode for a transaction.
- **Certificates** (1 message): querying the revocation-chain status of a certificate.

The nested type definitions used *inside* these 27 new messages were not audited for this note — see Limitations.

## The exceptions

This is the part that matters if you're deciding whether "OCPP 2.1 is backward compatible" is true for your integration. It mostly is — but not unconditionally.

### 1. The one genuine break: a new required field

`NotifyMonitoringReportRequest`'s nested `VariableMonitoringType` object gains a new **required** field, `eventNotificationType`, backed by a brand-new enum `EventNotificationEnumType`. This is the only place in the entire common message set where a `required` array grows.

Before (OCPP 2.0.1):
```json
"VariableMonitoringType": {
  "required": ["id", "transaction", "value", "type", "severity"]
}
```

After (OCPP 2.1):
```json
"VariableMonitoringType": {
  "required": ["id", "transaction", "value", "type", "severity", "eventNotificationType"]
},
"EventNotificationEnumType": {
  "type": "string",
  "enum": ["HardWiredNotification", "HardWiredMonitor", "PreconfiguredMonitor", "CustomMonitor"]
}
```

A `VariableMonitoringType` object built by 2.0.1-era code — e.g. anything constructing a `NotifyMonitoringReport` from existing monitoring logic — is schema-invalid under OCPP 2.1 unless it's updated to populate this field. This affects senders (charging stations reporting monitoring events), not receivers.

### 2. Three closed enums became open strings

`IdTokenType.type`, `chargingLimitSource`, and `connectorType` are each backed, in 2.0.1, by a closed JSON-Schema `enum`. In 2.1, the enum type definition is deleted outright and the field becomes a plain `{"type": "string", "maxLength": 20}`, with a description pointing to an appendix ("Values defined in Appendix as XEnumStringType") instead of a JSON-Schema-enforced list.

- `IdTokenEnumType` (2.0.1 values: `Central`, `eMAID`, `ISO14443`, `ISO15693`, `KeyCode`, `Local`, `MacAddress`, `NoAuthorization`) → removed; `IdTokenType.type` becomes an open string. Affects `Authorize`, `TransactionEvent` (Request and Response), `RequestStartTransaction`, `ReserveNow`, `SendLocalList`, `CustomerInformation`.
- `ChargingLimitSourceEnumType` (2.0.1 values: `EMS`, `Other`, `SO`, `CSO`) → removed; the field becomes an open string. Affects `ClearedChargingLimit`, `GetChargingProfiles`, `NotifyChargingLimit`, `ReportChargingProfiles`.
- `ConnectorEnumType` → removed; `connectorType` becomes an open string. Affects `ReserveNow`.

This is non-breaking for a sender that only ever emits values from the old closed list — those values are still valid strings. It is breaking for: (a) any receiver doing strict JSON-Schema enum validation against the old schema, (b) any application code that exhaustively switches or pattern-matches on the enum's known members without a default/unknown case, and (c) any code generator that materializes these fields as a closed enum type at compile time (an enum-typed field, a Java `enum`, a Go `iota`-style const set, etc.) rather than a plain string — such generated types will reject values outside the old list even though the wire format now permits them.

### 3. Three required fields became optional

- `ChargingSchedulePeriodType.limit`: required → optional, in `GetCompositeScheduleResponse`, `NotifyChargingLimitRequest`, `NotifyEVChargingScheduleRequest`, `ReportChargingProfilesRequest`, `RequestStartTransactionRequest`, `SetChargingProfileRequest` (6 files). This goes with a broader change: `ChargingSchedulePeriodType` also gains 17 new optional fields in 2.1 (`setpoint`, `dischargeLimit`, `v2xBaseline`, `operationMode`, and others), reflecting the new V2X/DER charging-profile model where a period may now be described by a set-point rather than a hard limit.
- `SignedMeterValueType.signingMethod` and `.publicKey`: required → optional, in `MeterValuesRequest` and `TransactionEventRequest`.
- `NetworkConnectionProfileType.ocppVersion`: required → optional, in `SetNetworkProfileRequest`. The 2.1 schema's description for this field now reads: *"This field is ignored, since the OCPP version to use is determined during the websocket handshake. The field is only kept for backwards compatibility with the OCPP 2.0.1 JSON schema."* — a documented deprecation, not an accident.

Non-breaking for a sender that keeps supplying these fields anyway. Breaking for a reader that assumes their presence and dereferences them without a nil/absence check — a real risk for statically-typed consumers that map "required" straight onto a non-nullable struct field.

### 4. Seventeen string fields got longer size limits

`maxLength` was raised — never lowered — on seventeen fields. Six sit on widely shared types: `StatusInfoType.additionalInfo` (512→1024, reaching the 42 response messages that carry a status detail), `AdditionalInfoType.additionalIdToken` (36→255), `IdTokenType.idToken` (36→255), `SignedMeterValueType.signedMeterData` (2500→32768), `MessageContentType.content` (512→1024), `OCSPRequestDataType.responderURL` (512→2000). Eight more cluster in one message, `SetNetworkProfile`, which raises every connection-credential field on `APNType` and `VPNType` (`apn` 512→2000, `apnUserName` 20→50, `apnPassword` 20→64, `server` 512→2000, `user` 20→50, `group` 20→50, `password` 20→64) plus `NetworkConnectionProfileType.ocppCsmsUrl` (512→2000). The last three are single-message value fields: `FirmwareType.location` (512→2000, `UpdateFirmware`), `LogParametersType.remoteLocation` (512→2000, `GetLog`), and `SetVariableDataType.attributeValue` (1000→2500, `SetVariables`). Non-breaking for senders. Relevant only if you have fixed-width storage — a database column, a buffer, a truncating log field — sized to an old limit. The full seventeen, with old→new pairs and affected messages, are rows 8–24 of Appendix A.

### 5. Twenty-two enumerations gained new values

Twenty-two of the closed value lists grew. Three account for most of the growth: `MeasurandEnumType` **+31** (almost all V2X/DER/display measurands such as `EnergyRequest.Target`, `Display.PresentSOC`, `Power.Active.Setpoint`), `TriggerReasonEnumType` **+8** (e.g. `TariffChanged`, `CostLimitReached`, `SoCLimitReached`), and `EnergyTransferModeEnumType` **+7** (bidirectional and wireless transfer modes such as `AC_BPT`, `DC_BPT`, `WPT`). Five gained two values each — `ChargingProfilePurposeEnumType`, `MessageStateEnumType`, `MessageTriggerEnumType`, `MonitorEnumType`, `OCPPVersionEnumType` — and the remaining fourteen gained one each, among them `LocationEnumType` (`Upstream`), `ReasonEnumType` (`ReqEnergyTransferRejected`), `MessageFormatEnumType` (`QRCODE`), `ChargingProfileKindEnumType` (`Dynamic`), and `ResetEnumType` (`ImmediateAndResume`). Every widened enum, with its exact new values and affected messages, is in Appendix A rows 25–46. A JSON-Schema `enum` is a closed list by construction, so these are widenings of a closed value set — and, like the enum removals in exception 2, the effect is directional. Validated against the **2.1** schema, the change is additive: every old value remains legal and the new ones become legal. Validated against the **2.0.1** schema — the live transition case, e.g. a 2.0.1 CSMS receiving one of the 31 new measurands from a 2.1 charging station — the new value fails schema validation and the message is rejected. Beyond schema validation, a widened set also breaks (a) application code that exhaustively `switch`/pattern-matches over the old value set with no default branch, and (b) code generators that materialize the old list as a closed compile-time enum type, since a previously-impossible value can now legally arrive on the wire.

### 6. One clean, no-caveat addition, for contrast

`NotifyReportRequest`'s nested `VariableCharacteristicsType` gains one new optional property, `maxElements`, with no change to `required`. This is the pattern the "additive" claim looks like when it's actually simple: a genuinely optional field, nothing else moved.

## Practical implications for anyone upgrading 2.0.1 logic to 2.1

What carries over untouched: the outer shape of every shared message — which fields are required, which are present, and their types — is identical. Code that only builds or parses the well-known 2.0.1 fields of the 64 shared messages needs no changes to keep validating against the 2.1 schemas.

What to audit, mapped to the exceptions above:

1. If you emit `NotifyMonitoringReport`, populate `eventNotificationType` on every `VariableMonitoringType` you construct. This is the only mandatory code change implied by the schema diff alone.
2. If you validate incoming `idToken.type`, `chargingLimitSource`, or `connectorType` against a closed value set, or generate strict enum types for them, relax that validation (or add an "unknown value" fallback) before accepting 2.1 traffic.
3. If you read `ChargingSchedulePeriodType.limit`, `SignedMeterValueType.signingMethod`/`publicKey`, or `NetworkConnectionProfileType.ocppVersion` without a presence check, add one.
4. If any of the seventeen size-limited fields (Appendix A rows 8–24) feed a fixed-width store, widen it to the new limit before accepting 2.1 payloads.
5. If any code path exhaustively matches on one of the twenty-two widened enumerations (Appendix A rows 25–46; `MeasurandEnumType` is the most consequential), give it a default/unknown-value branch — and if you validate inbound values against the 2.0.1 lists (in-schema or via generated enum types), widen them to the 2.1 sets before accepting 2.1 traffic.
6. If any tooling assumes every OCPP action has both a Request and a Response schema, special-case `NotifyPeriodicEventStream` — and, more generally, design for the new `SEND` (fire-and-forget) and `CALLRESULTERROR` frame types at the RPC layer, not just at the Part 3 schema layer.

## Limitations

- The nested type definitions *used inside* the 27 brand-new OCPP 2.1 messages were not audited here — this note only enumerates the new messages by name and theme, and separately, exhaustively diffs the nested definitions of the 64 *shared* messages. A new message's own internal types were out of scope.
- This is a JSON-Schema (Part 3) diff. It does not cover the RPC/framing layer (Part 4). OCPP 2.1 adds two new frame types beyond 2.0.1's `CALL`/`CALLRESULT`/`CALLERROR`: `CALLRESULTERROR` (an error response to a CALLRESULT) and `SEND` (an unconfirmed, one-way message). "Schema-additive" does not mean "wire-protocol-identical" — a fully 2.0.1-compliant WebSocket/RPC implementation will not recognize these two new frame types without a code change, independent of anything in this note's schema analysis.
- Merge-patch-style schema overrides, if any exist outside the raw Part 3 ZIP, were not examined; this note diffs only the JSON files shipped in each edition's Part 3 bundle.

## Copyright note

The OCPP 2.0.1 and OCPP 2.1 specification documents, including the Part 3 JSON schemas referenced throughout, are copyright © Open Charge Alliance and distributed under the Creative Commons Attribution-NoDerivatives 4.0 International Public License. Only minimal excerpts are quoted here for the purpose of comparison; the full specification bundles, schemas included, are downloadable free of charge and without registration from `openchargealliance.org/my-oca/ocpp/` (verified 2026-08-09: both editions' "all files" links serve anonymously).

---

## Appendix A: full exceptions list

| # | Change class | Definition | Field(s) | Affected messages |
|---|---|---|---|---|
| 1 | Breaking requiredness addition | `VariableMonitoringType` | `eventNotificationType` (new required field; new `EventNotificationEnumType`) | `NotifyMonitoringReport` |
| 2 | Enum → open string | `IdTokenEnumType` (removed) | `IdTokenType.type` | `Authorize`, `TransactionEvent`, `RequestStartTransaction`, `ReserveNow`, `SendLocalList`, `CustomerInformation` |
| 3 | Enum → open string | `ChargingLimitSourceEnumType` (removed) | `chargingLimitSource` | `ClearedChargingLimit`, `GetChargingProfiles`, `NotifyChargingLimit`, `ReportChargingProfiles` |
| 4 | Enum → open string | `ConnectorEnumType` (removed) | `connectorType` | `ReserveNow` |
| 5 | Requiredness relaxation | `ChargingSchedulePeriodType` | `limit` (required → optional); +17 new optional fields | `GetCompositeSchedule` (resp.), `NotifyChargingLimit`, `NotifyEVChargingSchedule`, `ReportChargingProfiles`, `RequestStartTransaction`, `SetChargingProfile` |
| 6 | Requiredness relaxation | `SignedMeterValueType` | `signingMethod`, `publicKey` (required → optional) | `MeterValues`, `TransactionEvent` |
| 7 | Requiredness relaxation + deprecation note | `NetworkConnectionProfileType` | `ocppVersion` (required → optional) | `SetNetworkProfile` |
| 8 | `maxLength` loosened | `StatusInfoType` | `additionalInfo` 512 → 1024 | the 42 response messages carrying `StatusInfoType` |
| 9 | `maxLength` loosened | `AdditionalInfoType` | `additionalIdToken` 36 → 255 | `Authorize` (req & resp), `CustomerInformation`, `RequestStartTransaction`, `ReserveNow`, `SendLocalList`, `TransactionEvent` (req & resp) |
| 10 | `maxLength` loosened | `IdTokenType` | `idToken` 36 → 255 | `Authorize` (req & resp), `CustomerInformation`, `RequestStartTransaction`, `ReserveNow`, `SendLocalList`, `TransactionEvent` (req & resp) |
| 11 | `maxLength` loosened | `SignedMeterValueType` | `signedMeterData` 2500 → 32768 | `MeterValues`, `TransactionEvent` |
| 12 | `maxLength` loosened | `MessageContentType` | `content` 512 → 1024 | `Authorize` (resp.), `NotifyDisplayMessages`, `SendLocalList`, `SetDisplayMessage`, `TransactionEvent` (resp.) |
| 13 | `maxLength` loosened | `OCSPRequestDataType` | `responderURL` 512 → 2000 | `Authorize`, `GetCertificateStatus` |
| 14 | `maxLength` loosened | `APNType` | `apn` 512 → 2000 | `SetNetworkProfile` |
| 15 | `maxLength` loosened | `APNType` | `apnUserName` 20 → 50 | `SetNetworkProfile` |
| 16 | `maxLength` loosened | `APNType` | `apnPassword` 20 → 64 | `SetNetworkProfile` |
| 17 | `maxLength` loosened | `VPNType` | `server` 512 → 2000 | `SetNetworkProfile` |
| 18 | `maxLength` loosened | `VPNType` | `user` 20 → 50 | `SetNetworkProfile` |
| 19 | `maxLength` loosened | `VPNType` | `group` 20 → 50 | `SetNetworkProfile` |
| 20 | `maxLength` loosened | `VPNType` | `password` 20 → 64 | `SetNetworkProfile` |
| 21 | `maxLength` loosened | `NetworkConnectionProfileType` | `ocppCsmsUrl` 512 → 2000 | `SetNetworkProfile` |
| 22 | `maxLength` loosened | `FirmwareType` | `location` 512 → 2000 | `UpdateFirmware` |
| 23 | `maxLength` loosened | `LogParametersType` | `remoteLocation` 512 → 2000 | `GetLog` |
| 24 | `maxLength` loosened | `SetVariableDataType` | `attributeValue` 1000 → 2500 | `SetVariables` |
| 25 | Enum value addition | `MeasurandEnumType` | +31 values (V2X/DER/display) | `MeterValues`, `TransactionEvent` |
| 26 | Enum value addition | `TriggerReasonEnumType` | +8 (`CostLimitReached`, `LimitSet`, `OperationModeChanged`, `RunningCost`, `SoCLimitReached`, `TariffChanged`, `TariffNotAccepted`, `TxResumed`) | `TransactionEvent` |
| 27 | Enum value addition | `EnergyTransferModeEnumType` | +7 (`AC_BPT`, `AC_BPT_DER`, `AC_DER`, `DC_ACDP`, `DC_ACDP_BPT`, `DC_BPT`, `WPT`) | `NotifyEVChargingNeeds` |
| 28 | Enum value addition | `ChargingProfilePurposeEnumType` | +2 (`LocalGeneration`, `PriorityCharging`) | `ClearChargingProfile`, `GetChargingProfiles`, `ReportChargingProfiles`, `RequestStartTransaction`, `SetChargingProfile` |
| 29 | Enum value addition | `MessageStateEnumType` | +2 (`Discharging`, `Suspended`) | `GetDisplayMessages`, `NotifyDisplayMessages`, `SetDisplayMessage` |
| 30 | Enum value addition | `MessageTriggerEnumType` | +2 (`CustomTrigger`, `SignV2G20Certificate`) | `TriggerMessage` |
| 31 | Enum value addition | `MonitorEnumType` | +2 (`TargetDelta`, `TargetDeltaRelative`) | `NotifyMonitoringReport`, `SetVariableMonitoring` (req & resp) |
| 32 | Enum value addition | `OCPPVersionEnumType` | +2 (`OCPP201`, `OCPP21`) | `SetNetworkProfile` |
| 33 | Enum value addition | `CertificateSigningUseEnumType` | +1 (`V2G20Certificate`) | `CertificateSigned`, `SignCertificate` |
| 34 | Enum value addition | `ChargingProfileKindEnumType` | +1 (`Dynamic`) | `ReportChargingProfiles`, `RequestStartTransaction`, `SetChargingProfile` |
| 35 | Enum value addition | `ClearMessageStatusEnumType` | +1 (`Rejected`) | `ClearDisplayMessage` (resp.) |
| 36 | Enum value addition | `DisplayMessageStatusEnumType` | +1 (`LanguageNotSupported`) | `SetDisplayMessage` (resp.) |
| 37 | Enum value addition | `GetCertificateIdUseEnumType` | +1 (`OEMRootCertificate`) | `GetInstalledCertificateIds` (req & resp) |
| 38 | Enum value addition | `InstallCertificateUseEnumType` | +1 (`OEMRootCertificate`) | `InstallCertificate` |
| 39 | Enum value addition | `LocationEnumType` | +1 (`Upstream`) | `MeterValues`, `TransactionEvent` |
| 40 | Enum value addition | `LogEnumType` | +1 (`DataCollectorLog`) | `GetLog` |
| 41 | Enum value addition | `MessageFormatEnumType` | +1 (`QRCODE`) | `Authorize` (resp.), `NotifyDisplayMessages`, `SendLocalList`, `SetDisplayMessage`, `TransactionEvent` (resp.) |
| 42 | Enum value addition | `NotifyEVChargingNeedsStatusEnumType` | +1 (`NoChargingProfile`) | `NotifyEVChargingNeeds` (resp.) |
| 43 | Enum value addition | `OCPPInterfaceEnumType` | +1 (`Any`) | `SetNetworkProfile` |
| 44 | Enum value addition | `ReasonEnumType` | +1 (`ReqEnergyTransferRejected`) | `TransactionEvent` |
| 45 | Enum value addition | `ReservationUpdateStatusEnumType` | +1 (`NoTransaction`) | `ReservationStatusUpdate` |
| 46 | Enum value addition | `ResetEnumType` | +1 (`ImmediateAndResume`) | `Reset` |
| 47 | Clean additive (for contrast) | `VariableCharacteristicsType` | +`maxElements` (optional) | `NotifyReport` |
| 48 | Naming/framing anomaly | n/a | No Request/Response suffix; uses the new `SEND` RPC frame type | `NotifyPeriodicEventStream` |

## Appendix B: the 27 new OCPP 2.1 messages

| Message | Theme |
|---|---|
| ClearDERControl | DER control |
| GetDERControl | DER control |
| SetDERControl | DER control |
| ReportDERControl | DER control |
| NotifyDERAlarm | DER control |
| NotifyDERStartStop | DER control |
| AFRRSignal | V2X / bidirectional and grid signals |
| NotifyAllowedEnergyTransfer | V2X / bidirectional and grid signals |
| PullDynamicScheduleUpdate | V2X / bidirectional and grid signals |
| UpdateDynamicSchedule | V2X / bidirectional and grid signals |
| BatterySwap | Battery swapping |
| RequestBatterySwap | Battery swapping |
| AdjustPeriodicEventStream | Periodic event streams |
| ClosePeriodicEventStream | Periodic event streams |
| GetPeriodicEventStream | Periodic event streams |
| NotifyPeriodicEventStream | Periodic event streams (SEND-framed, unpaired) |
| OpenPeriodicEventStream | Periodic event streams |
| ChangeTransactionTariff | Tariffs / cost / payment |
| ClearTariffs | Tariffs / cost / payment |
| GetTariffs | Tariffs / cost / payment |
| SetDefaultTariff | Tariffs / cost / payment |
| NotifySettlement | Tariffs / cost / payment |
| NotifyWebPaymentStarted | Tariffs / cost / payment |
| VatNumberValidation | Tariffs / cost / payment |
| NotifyPriorityCharging | Priority charging |
| UsePriorityCharging | Priority charging |
| GetCertificateChainStatus | Certificates |
