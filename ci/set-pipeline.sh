#!/usr/bin/env bash
set -e

DIR=$( cd "$( dirname "$0" )" && pwd )

# Update following line with correct pipeline
PIPELINE=rds_exporter_hf

#############################################################

# Ensure we have a fly target
# The target is depending on how you named the fly target during 'fly --login ...'.
if [ -z "${FLY_TARGET}" ]; then
    echo "Missing FLY_TARGET environment variable"
    exit 1
fi

# Load pipeline vars
VARS_FILE="${DIR}/assets/vars.yml"
if [ ! -f "${VARS_FILE}" ]; then
    echo "Missing vars.yml file"
    exit 1
fi

# Set the pipeline
fly -t "${FLY_TARGET}" set-pipeline -p "${PIPELINE}" -c "${DIR}/pipeline.yml" --load-vars-from="${VARS_FILE}"
