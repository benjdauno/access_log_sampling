package volumebasedlogsampler
// Switch to package main to run this file independently and test snowflake
//package main
import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"

	"database/sql"

	"github.com/snowflakedb/gosnowflake"
	//"go.opentelemetry.io/collector/pdata/internal/data"
)

// LoadPrivateKey reads and parses an RSA private key file
func LoadPrivateKey(filePath string) (*rsa.PrivateKey, error) {
	keyData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	rsaKey, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}

	return rsaKey, nil
}

// ConnectToSnowflake initializes a Snowflake connection
func ConnectToSnowflake() (*sql.DB, error) {
	// Define Snowflake connection details
	account := "affirmuseast.us-east-1"
	user := "benjamin.daunoravicius@affirm.com"
	privateKeyPath := "/workspaces/access_log_sampling/rsa_key_benjdaun_snowflake_fixed.p8"
	database := "PROD__US"
	schema := "PUBLIC"

	// Load the private key
	privateKey, err := LoadPrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}

	// Construct Snowflake DSN with key-pair authentication
	dsn, err := gosnowflake.DSN(&gosnowflake.Config{
		Account:       account,
		User:          user,
		Authenticator: gosnowflake.AuthTypeJwt,
		PrivateKey:    privateKey,
		Database:      database,
		Schema:        schema,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create DSN: %w", err)
	}

	// Open the database connection
	db, err := sql.Open("snowflake", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open Snowflake connection: %w", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping Snowflake: %w", err)
	}

	fmt.Println("Connected to Snowflake successfully!")
	return db, nil
}

func main() {
	db, err := ConnectToSnowflake()
	if err != nil {
		log.Fatalf("Error connecting to Snowflake: %v", err)
	}
	defer db.Close()

	// Example query
	minCheckouts := 200
	query := fmt.Sprintf(`
	SELECT merchant_dim.merchant_ari,
		count(*) as num_checkouts
	FROM dbt_analytics.checkout_session_fact AS checkout_funnel_base
	LEFT JOIN dbt_analytics.charge_fact AS charge_fact ON (checkout_funnel_base."CHECKOUT_ARI") = (charge_fact."CHECKOUT_ARI")
	LEFT JOIN DBT_ANALYTICS."MERCHANT_DIM" AS merchant_dim ON (checkout_funnel_base."MERCHANT_ARI") = (merchant_dim."MERCHANT_ARI")
	WHERE charge_fact.IS_AUTHED = 1
		AND checkout_funnel_base."CHECKOUT_CREATED_DT" >= DATEADD(month, -1, CURRENT_DATE()) 
		AND checkout_funnel_base."CHECKOUT_CREATED_DT" < CURRENT_DATE()
	GROUP BY 1
	HAVING count(*) > %d
	ORDER BY 2 desc;
`, minCheckouts)
	rows, err := db.Query(query)
	if err != nil {
		log.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	// Print the results
	var merchantARI string
	var numCheckouts int
	for rows.Next() {
		err := rows.Scan(&merchantARI, &numCheckouts)
		if err != nil {
			log.Fatalf("Failed to scan row: %v", err)
		}
		fmt.Printf("Merchant ARI: %s, Number of Checkouts: %d\n", merchantARI, numCheckouts)
	}
}
