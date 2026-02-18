#!/usr/bin/env bash
set -euo pipefail

# Configuration
TOKEN="${NETBOX_TOKEN:-}"
NETBOX_URL="${NETBOX_URL:-http://localhost:8080}"
OUTPUT_DIR="./netbox_ips_dump"
LIMIT=1000  # Fetch 1000 IPs per request (adjust if needed)

# Strip trailing slash from URL
NETBOX_URL="${NETBOX_URL%/}"
API_URL="${NETBOX_URL}/api/ipam/ip-addresses/"

# Check if token is set
if [ -z "$TOKEN" ]; then
    echo "Error: NETBOX_TOKEN environment variable is not set"
    echo ""
    echo "Usage:"
    echo "  NETBOX_TOKEN='your-token' $0"
    echo "  NETBOX_TOKEN='your-token' NETBOX_URL='http://netbox.example.com' $0"
    echo ""
    echo "Environment variables:"
    echo "  NETBOX_TOKEN  - Required: Netbox API token"
    echo "  NETBOX_URL    - Optional: Netbox base URL (default: http://localhost:8080)"
    exit 1
fi

# Create output directory
mkdir -p "$OUTPUT_DIR"

echo "Fetching IP addresses from NetBox..."
echo "  URL: $API_URL"
echo ""

offset=0
page=1
total_fetched=0

while true; do
    echo "Fetching page $page (offset: $offset)..."

    # Make the request
    response=$(curl -s -H "Authorization: Token $TOKEN" \
        "${API_URL}?limit=${LIMIT}&offset=${offset}")

    # Save raw response to file
    output_file="$OUTPUT_DIR/page_$(printf '%04d' $page).json"
    echo "$response" > "$output_file"

    # Parse the response to check if we have more data
    count=$(echo "$response" | jq -r '.count // 0')
    results_count=$(echo "$response" | jq -r '.results | length')

    total_fetched=$((total_fetched + results_count))

    echo "  - Fetched $results_count IPs (total: $total_fetched / $count)"
    echo "  - Saved to: $output_file"

    # Check if we've fetched everything
    if [ "$total_fetched" -ge "$count" ] || [ "$results_count" -eq 0 ]; then
        break
    fi

    # Move to next page
    offset=$((offset + LIMIT))
    page=$((page + 1))
done

echo ""
echo "Done! Fetched $total_fetched IP addresses"
echo "Raw responses saved to: $OUTPUT_DIR/"
echo ""
echo "To combine all results into a single JSON array:"
echo "  jq -s '[.[].results[]]' $OUTPUT_DIR/page_*.json > all_ips.json"
