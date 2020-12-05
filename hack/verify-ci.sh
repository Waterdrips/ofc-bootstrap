#! /bin/sh

set -euf -o pipefail

FILE=/tmp/helm_list.json

helm ls -A -o json > $FILE
cat $FILE | jq '.[] | select(.name=="cert-manager").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="cloud-minio").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="nginxingress").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="ofc-sealedsecrets").status' | xargs -I % test % = "deployed"
cat $FILE | jq '.[] | select(.name=="openfaas").status' | xargs -I % test % = "deployed"
