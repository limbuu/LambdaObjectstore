#!/bin/bash

PWD=`dirname $0`
PREFIX="Proxy2Node"
KEY="redeo_lambda"
cluster=10
mem=1024

echo "compiling lambda code..."
GOOS=linux go build $KEY.go
echo "compress file..."
zip $KEY $KEY
echo "updating lambda code.."

echo "putting code zip to s3"
#aws s3api put-object --bucket ao.lambda.code --key $KEY.zip --body $KEY.zip
aws s3api put-object --bucket ao.code.lambda --key $KEY.zip --body $KEY.zip

go run $PWD/../../sbin/deploy_function.go -create=true -code=true -config=true -prefix=$PREFIX -vpc=false -key=$KEY -to=$cluster -mem=$mem -timeout=$1
go clean
