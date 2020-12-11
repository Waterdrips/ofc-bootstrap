#! /bin/bash

set -euf

FILE=/tmp/helm_list.json

# Check the secrets are created (ones in example_init.yaml)

# Check ofc functions are installed, ones from stack.yaml

# Check github functions are installed

# Check core components are installed (edge-auth etc)

# Check the gateway is up (using a port forward)

# Check the dashboard is up


# Check helm thinks the components are installed and deployed
helm ls -A -o json > $FILE
cat $FILE | jq '.[] | select(.name=="cert-manager").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="cloud-minio").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="nginxingress").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="ofc-sealedsecrets").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="openfaas").status' | xargs -I % test % = "deployed"
