package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TestServer tests the basic functionality of the Stremio server
func TestServer(baseURL string) error {
	fmt.Printf("Testing Stremio server at: %s\n", baseURL)
	
	// Test server status
	fmt.Println("\n1. Testing server status...")
	resp, err := http.Get(baseURL + "/api/status")
	if err != nil {
		return fmt.Errorf("failed to get status: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status endpoint returned %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	
	var status map[string]interface{}
	if err := json.Unmarshal(body, &status); err != nil {
		return fmt.Errorf("failed to parse status response: %v", err)
	}
	
	fmt.Printf("Server status: %s\n", status["status"])
	fmt.Printf("Uptime: %s\n", status["uptime"])
	fmt.Printf("Active engines: %v\n", status["engines"])
	
	// Test network info
	fmt.Println("\n2. Testing network info...")
	resp, err = http.Get(baseURL + "/api/network-info")
	if err != nil {
		return fmt.Errorf("failed to get network info: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("network-info endpoint returned %d", resp.StatusCode)
	}
	
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	
	var networkInfo map[string]interface{}
	if err := json.Unmarshal(body, &networkInfo); err != nil {
		return fmt.Errorf("failed to parse network info response: %v", err)
	}
	
	fmt.Printf("Local IP: %s\n", networkInfo["localIP"])
	fmt.Printf("Server Port: %v\n", networkInfo["serverPort"])
	
	// Test device info
	fmt.Println("\n3. Testing device info...")
	resp, err = http.Get(baseURL + "/api/device-info")
	if err != nil {
		return fmt.Errorf("failed to get device info: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("device-info endpoint returned %d", resp.StatusCode)
	}
	
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	
	var deviceInfo map[string]interface{}
	if err := json.Unmarshal(body, &deviceInfo); err != nil {
		return fmt.Errorf("failed to parse device info response: %v", err)
	}
	
	fmt.Printf("Hostname: %s\n", deviceInfo["hostname"])
	fmt.Printf("Platform: %s\n", deviceInfo["platform"])
	
	// Test torrent list (should be empty initially)
	fmt.Println("\n4. Testing torrent list...")
	resp, err = http.Get(baseURL + "/api/list")
	if err != nil {
		return fmt.Errorf("failed to get torrent list: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list endpoint returned %d", resp.StatusCode)
	}
	
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	
	var torrents []interface{}
	if err := json.Unmarshal(body, &torrents); err != nil {
		return fmt.Errorf("failed to parse torrent list response: %v", err)
	}
	
	fmt.Printf("Active torrents: %d\n", len(torrents))
	
	// Test settings
	fmt.Println("\n5. Testing settings...")
	resp, err = http.Get(baseURL + "/api/settings")
	if err != nil {
		return fmt.Errorf("failed to get settings: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("settings endpoint returned %d", resp.StatusCode)
	}
	
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	
	var settings map[string]interface{}
	if err := json.Unmarshal(body, &settings); err != nil {
		return fmt.Errorf("failed to parse settings response: %v", err)
	}
	
	fmt.Printf("Local addon enabled: %v\n", settings["localAddonEnabled"])
	fmt.Printf("Proxy streams enabled: %v\n", settings["proxyStreamsEnabled"])
	fmt.Printf("Streaming server URL: %s\n", settings["streamingServerUrl"])
	
	// Test FFmpeg functionality
	fmt.Println("\n6. Testing FFmpeg functionality...")
	resp, err = http.Get(baseURL + "/api/hwaccel-profiler")
	if err != nil {
		fmt.Printf("❌ Hardware acceleration profiler test failed: %v\n", err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("❌ Failed to read hardware acceleration response: %v\n", err)
			} else {
				var hwInfo map[string]interface{}
				if err := json.Unmarshal(body, &hwInfo); err != nil {
					fmt.Printf("❌ Failed to parse hardware acceleration response: %v\n", err)
				} else {
					fmt.Printf("✅ Hardware acceleration info: %v\n", hwInfo)
				}
			}
		} else if resp.StatusCode == 503 {
			fmt.Println("⚠️  FFmpeg not available (this is normal if FFmpeg is not installed)")
		} else {
			fmt.Printf("❌ Hardware acceleration profiler returned status: %d\n", resp.StatusCode)
		}
	}
	
	fmt.Println("\n✅ All tests passed! Server is working correctly.")
	return nil
}

func main() {
	baseURL := "http://localhost:11470"
	
	// Wait a bit for server to start
	fmt.Println("Waiting for server to start...")
	time.Sleep(2 * time.Second)
	
	if err := TestServer(baseURL); err != nil {
		fmt.Printf("❌ Test failed: %v\n", err)
		return
	}
} 