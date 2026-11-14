URL: https://docs.skerrywind.example/v4/units
Title: Units and fields - Skerrywind telemetry API v4

# Units and fields

Version 4 sends every measurement as an **integer** in a fixed unit, so that no two
stations disagree about rounding.

| Member | Measurement | Unit | From common units |
|--------|-------------|------|-------------------|
| `air_temp_dk` | Air temperature | decikelvin (tenths of a kelvin) | (°C + 273.15) × 10 |
| `station_pres_dapa` | Station-level pressure | decapascals (tens of pascals) | hPa × 10 |
| `wind_speed_cms` | Mean wind speed | centimetres per second | m/s × 100 |

Each sample also has `taken_utc`, the RFC 3339 UTC time the sensors were read.

Convert, then round to the nearest integer. For example 12.34 °C is 285.49 K, so
`air_temp_dk` is `2855`; 1013.27 hPa is 101327 Pa, so `station_pres_dapa` is `10133`;
4.2 m/s is `wind_speed_cms` `420`.

A value that is not a JSON integer (including `2855.0`) is rejected with `422` and the
error code `units.not_integer`.

## Changes from the version 4 beta

The version 4 beta took pressure as `pressure_pa`, in pascals. The released version 4
replaced it with `station_pres_dapa`, in decapascals, so that station pressure fits the
same five digits as the archive's other columns; `pressure_pa` is now rejected with `422`
and the error code `samples.unknown_field`.

## Changes from version 3

Version 3 took floating-point `temp_c`, `pressure_hpa` and `wind_ms` and a `time` member.
Version 4 rejects those names with `422` and the error code `samples.unknown_field`.
