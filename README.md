# Fiorano-to-WSO2-APIM-Migrator

A small CLI tool for migrating a Fiorano API Management REST export into WSO2 API Manager — split the
exported project per backend, create and publish one WSO2 API per backend, combine them into a single
API Product, and push backend endpoint changes later, all driven by two plain properties files instead
of a pile of one-off scripts.

This is not an official WSO2 product. It exists to remove the manual, repetitive Publisher REST API work
that migrating a multi-backend Fiorano project otherwise requires.

## Folder layout

Run the binary from inside `bin/` — it finds `conf/` automatically next to it, no matter what directory
you're in:

```
<tool root>/
  bin/
    fiorano-to-wso2-apim-migrator-<platform>      <- run this
  conf/
    config.properties                 <- copy from config.properties.example and edit once
    backends.properties                <- created and updated by the tool
  logs/
    <projectName>.log                 <- created automatically; every run of every
                                          subcommand for this project appends here
```

## Commands

Every value below can also come from `config.properties`, so most days you just run the bare subcommand
with no flags at all:

```
fiorano-to-wso2-apim-migrator split            [<fiorano_project_dir>] [--out dir] [--version X.Y] [--config FILE]
fiorano-to-wso2-apim-migrator create-apis      [--config FILE] [--backends FILE] [--split-dir DIR] [--project-name NAME] [--mock-base-url URL] [--api-version X.Y]
fiorano-to-wso2-apim-migrator create-product   [--config FILE] [--backends FILE] [--split-dir DIR] [--project-name NAME] [--product-name NAME] [--product-context PATH] [--product-version X.Y]
fiorano-to-wso2-apim-migrator update-backends  [--config FILE] [--backends FILE]
```

A flag you pass always wins; `config.properties` only fills in what you leave out.

## config.properties

```
# --- Server connection (required) ---
host=your-wso2-host
adminUser=admin
adminPassword=yourpassword
environment=Default
vhost=your-wso2-host

# --- Project settings (used by split, create-apis, create-product) ---
projectDir=/path/to/your-fiorano-project
outDir=/path/to/output
splitDir=/path/to/output/split-by-endpoint
projectName=YourProjectName
backendsFile=backends.properties
apiVersion=1.0
mockBaseUrl=http://localhost:8290/mock/your-prefix

# --- API Product settings (used by create-product) ---
productName=YourProjectName
productContext=/YourProjectName
productVersion=1.0

# --- Added automatically after the first run — leave these alone ---
clientId=
clientSecret=
```

`clientId`/`clientSecret` are obtained automatically via Dynamic Client Registration the first time any
command runs, and written back into this file for you.

## backends.properties

Created and updated by the tool itself — holds no secrets, just ids/URLs, so it's safe to share while
backend addresses trickle in:

```
api.<key>.name=...
api.<key>.context=...
api.<key>.id=...
api.<key>.backend=...   (leave blank until you have the real address; update-backends skips blanks)
product.name / product.context / product.id   (added by create-product)
```

## Logs

`logs/<projectName>.log` is created automatically the first time any subcommand runs for that project.
Every later run, by any subcommand, appends to the same file rather than overwriting it, so it builds a
full history of everything that's been done to that project.

## Building from source

```
GOOS=darwin  GOARCH=arm64 go build -o bin/fiorano-to-wso2-apim-migrator-macos-arm64   main.go
GOOS=darwin  GOARCH=amd64 go build -o bin/fiorano-to-wso2-apim-migrator-macos-amd64   main.go
GOOS=linux   GOARCH=amd64 go build -o bin/fiorano-to-wso2-apim-migrator-linux-amd64   main.go
GOOS=windows GOARCH=amd64 go build -o bin/fiorano-to-wso2-apim-migrator-windows-amd64.exe main.go
```
