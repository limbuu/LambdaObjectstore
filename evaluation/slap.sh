#!/bin/bash

if [ "$GOPATH" == "" ] ; then
  echo "No \$GOPATH defined. Install go and set \$GOPATH first."
fi

PWD=`dirname $0`
ENTRY=`date "+%Y%m%d%H%M"`
ENTRY="/data/$ENTRY"
NODE_PREFIX="Store1VPCNode"

source $PWD/util.sh

OPTIND=1         # Reset in case getopts has been used previously in the shell.
MEM=128
NUMBER=1
CON=1
KEYMIN=1
KEYMAX=2
SZ=128
DATA=4
PARITY=2
TIME=$1
while getopts ":hm:n:c:a:b:s:d:p:t:" opt; do
    case ${opt} in
    h)
#        echo "grade [options] tarfile"
#        echo "options:"
#        echo "  -m move file to the sub directory, which is named by 'date', of 'grade' program"
        exit 0
        ;;
    m)  MEM=$OPTARG
        ;;
    n)  NUMBER=$OPTARG
        ;;
    c)  CON=$OPTARG
        ;;
    a)  KEYMIN=$OPTARG
        ;;
    b)  KEYMAX=$OPTARG
        ;;
    s)  SZ=$OPTARG
        ;;
    d)  DATA=$OPTARG
        ;;
    p)  PARITY=$OPTARG
        ;;
    t)  TIME=$OPTARG
        ;;
    \?)
        ;;
    esac
done
shift $((OPTIND-1))

function perform(){
    MEM=$1
    NUMBER=$2
    CON=$3
    KEYMIN=$4
    KEYMAX=$5
    SZ=$6
    DATA=$7
    PARITY=$8
    TIME=$9
#    echo $i"_"DATA$P"_"lambda$MEM"_"$SZ

    for i in {1..5}
    do
        PREPROXY=$PWD/$ENTRY/No.$i"_"$DATA"_"$PARITY"_"lambda$MEM"_"$SZ
        PRESET=$PWD/$ENTRY/No.$i"_"$DATA"_"$PARITY"_"lambda$MEM"_"$SZ"_SET"
        PREGET=$PWD/$ENTRY/No.$i"_"$DATA"_"$PARITY"_"lambda$MEM"_"$SZ"_GET"

        update_lambda_timeout $NODE_PREFIX $((TIME+i*10))
        start_proxy $PREPROXY &
        while [ ! -f /tmp/lambdaproxy.pid ]
        do
            sleep 1s
        done
        cat /tmp/lambdaproxy.pid
#        set
        sleep 1s
        bench $((KEYMAX-KEYMIN+1)) 1 $KEYMIN $KEYMAX $SZ $DATA $PARITY 0 $PRESET
#        while [ ! -f /var/run/lambdaproxy.pid ]
#        do
#            sleep 1s
#        done
        sleep 1s
#        get
        bench $NUMBER $CON $KEYMIN $KEYMAX $SZ $DATA $PARITY 1 $PREGET
        kill -2 `cat /tmp/lambdaproxy.pid`
    done
}

#perform $*
#perform

MEMSET=(128 256 512 1024 1536 2048 3008)
SZSET=(10485760 20971520 41943040 62914020 83886080 104857600)
DS=(4 5 10 10 10)
PS=(2 1 1 2 4)
C=(1)
N=(10)
TIMEOUT=150
if [ "$1" != "" ]; then
    TIMEOUT="$1"
fi

mkdir -p $PWD/$ENTRY
for mem in {0..6}
do
    update_lambda_mem $NODE_PREFIX ${MEMSET[mem]}
    for sz in {0..5}
    do
        for k in {0..4}
        do
            for l in 0
            do
                # perform mem          num     concur keymin keymax objsize   datashards parity timeout
                perform ${MEMSET[mem]} ${N[l]} ${C[l]} 1 ${N[l]} ${SZSET[sz]} ${DS[k]} ${PS[k]} $TIMEOUT
            done
        done
    done
done

mv $PWD/log $PWD/$ENTRY.log
