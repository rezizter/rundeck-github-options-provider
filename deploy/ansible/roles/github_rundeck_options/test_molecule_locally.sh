#!/bin/bash
# 1. Build the Linux binary referenced by molecule.yml
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
  -o dist/github-rundeck-options-linux-amd64 main.go

# 2. Install Molecule & Testinfra dependencies
pip3 install molecule molecule-plugins[docker] pytest-testinfra

# 3. Run full test matrix (create -> converge -> idempotence -> verify -> destroy)
cd deploy/ansible/roles/github_rundeck_options
molecule test
