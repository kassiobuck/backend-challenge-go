#!/bin/sh
set -eu

DLQ_NAME="${SQS_DLQ_NAME:-wager-transactions-dlq.fifo}"
QUEUE_NAME="${SQS_QUEUE_NAME:-wager-transactions.fifo}"
OUTPUT_QUEUE_NAME="${SQS_OUTPUT_QUEUE_NAME:-wager-events.fifo}"
MAX_RECEIVE_COUNT="${SQS_MAX_RECEIVE_COUNT:-5}"

DLQ_URL="$(awslocal sqs create-queue \
  --queue-name "$DLQ_NAME" \
  --attributes '{"FifoQueue":"true","ContentBasedDeduplication":"false"}' \
  --query 'QueueUrl' \
  --output text)"
DLQ_ARN="$(awslocal sqs get-queue-attributes \
  --queue-url "$DLQ_URL" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' \
  --output text)"

REDRIVE_POLICY="$(python3 -c 'import json,sys; print(json.dumps({"deadLetterTargetArn":sys.argv[1],"maxReceiveCount":sys.argv[2]}))' \
  "$DLQ_ARN" "$MAX_RECEIVE_COUNT")"
awslocal sqs create-queue \
  --queue-name "$QUEUE_NAME" \
  --attributes "$(python3 -c 'import json,sys; print(json.dumps({"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"30","RedrivePolicy":sys.argv[1]}))' "$REDRIVE_POLICY")" \
  --output text >/dev/null

awslocal sqs create-queue \
  --queue-name "$OUTPUT_QUEUE_NAME" \
  --attributes '{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"30"}' \
  --output text >/dev/null

echo "Provisioned SQS FIFO queues $QUEUE_NAME and $OUTPUT_QUEUE_NAME; inbound DLQ: $DLQ_NAME"
