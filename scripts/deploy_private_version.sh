#!/bin/bash

# deploy_private_version.sh - Performs a complete build/packaging of a specific branch, for specified platforms.
#
# Syntax:   deploy_private_version -c <channel> [ -g <genesis-network> | -f <genesis-file> ] -n <network>
#
# Outputs:  <errors or warnings>
#
# ExitCode: 0 = Success - new version built and uploaded
#
# Usage:    Can be used locally to publish a local build for testing
#
# Examples: scripts/deploy_private_version.sh -c TestCatchup -g testnet -n testnetwork
#
# Notes:    If you're running on a Mac, this will attempt to use docker to build for linux.
#           GenesisNetwork currently must be either testnet or devnet -- use -f for a custom genesis.json file

set -e

export GOPATH=$(go env GOPATH)
export SRCPATH=${GOPATH}/src/github.com/algorand/go-algorand
cd ${SRCPATH}

CHANNEL=""
DEFAULTNETWORK=""
NETWORK=""
GENESISFILE=""
BUCKET=""

while [ "$1" != "" ]; do
    case "$1" in
        -c)
            shift
            CHANNEL=$1
            ;;
        -g)
            shift
            DEFAULTNETWORK=$1
            ;;
        -n)
            shift
            NETWORK=$1
            ;;
        -f)
            shift
            GENESISFILE=$1
            ;;
        -b)
            shift
            BUCKET=$1
            ;;
        *)
            echo "Unknown option" "$1"
            exit 1
            ;;
    esac
    shift
done

if [[ "${CHANNEL}" = "" || "${NETWORK}" = "" || "${DEFAULTNETWORK}" = "" && "${GENESISFILE}" = "" ]]; then
    echo "Syntax: deploy_private_version -c <channel> [ -g <genesis-network> | -f <genesis-file> ] -n <network>"
    echo "e.g. deploy_private_version.sh -c TestCatchup -g testnet -n testnetwork"
    exit 1
fi

# If GENESISFILE specified, DEFAULTNETWORK doesn't really matter but we need to ensure we have one
if [[ "${DEFAULTNETWORK}" = "" ]]; then
    DEFAULTNETWORK=devnet
elif [[ "${DEFAULTNETWORK}" != "devnet" && "${DEFAULTNETWORK}" != "testnet" ]]; then
    echo "genesis-network needs to be either devnet or testnet"
    exit 1
fi

export BRANCH=$(./scripts/compute_branch.sh)
export CHANNEL=${CHANNEL}
export BUILDCHANNEL=${CHANNEL}
export DEFAULTNETWORK=${DEFAULTNETWORK}
export FULLVERSION=$(./scripts/compute_build_number.sh -f)
export PKG_ROOT=${HOME}/node_pkg

if [[ $(uname) == "Darwin" ]]; then
    export NETWORK=${NETWORK}
    export GENESISFILE=${GENESISFILE}
    scripts/deploy_linux_version.sh -t ${SRCPATH}/tmp/${NETWORK}
    exit
fi

# modify genesis.json to use a custom network name to prevent SRV record resolving
TEMPDIR=$(mktemp -d 2>/dev/null || mktemp -d -t "tmp")
cp gen/${DEFAULTNETWORK}/genesis.json ${TEMPDIR}
trap "cp ${TEMPDIR}/genesis.json gen/${DEFAULTNETWORK};rm -rf ${TEMPDIR}" 0
if [[ "${GENESISFILE}" = "" ]]; then
    sed "s/${DEFAULTNETWORK}/${NETWORK}/" ${TEMPDIR}/genesis.json > gen/${DEFAULTNETWORK}/genesis.json
else
    cp ${GENESISFILE} gen/${DEFAULTNETWORK}/genesis.json
fi

# For private builds, always build the base version (with telemetry)
export VARIATIONS="base"
scripts/build_packages.sh $(./scripts/osarchtype.sh)

scripts/upload_version.sh ${CHANNEL} ${PKG_ROOT} ${BUCKET}
