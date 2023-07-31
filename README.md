# vault-secrets-reloader controller

## Description

A tool to watch secret changes in Vault since deploying a workload that uses them via `vault-secrets-webhook`, and "reloads" them so the `webhook` injects the latest secret versions into the objects.
