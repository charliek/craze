URL: https://docs.skerrywind.example/v4/station-keys
Title: Station keys - Skerrywind telemetry API v4

# Station keys

Each station has one active signing secret at a time, plus any number of read tokens.

## Creating and rotating a signing secret

In the console, open **Stations → Keys** for the station and choose **New signing
secret**. The secret is shown once; store it on the station's gateway. Secrets look like
`sws_` followed by 40 characters.

When you rotate a secret, the old one keeps working for 24 hours so gateways can be
updated in turn. After that, batches signed with it are refused.

## Read tokens

Read tokens start `swr_` and are sent as `Authorization: Bearer <token>` to the read host.
They cannot upload. A read token may be limited to some stations.

## If a secret leaks

Rotate it at once and choose **Revoke immediately** to skip the 24-hour overlap.
