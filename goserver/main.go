package main

import (
	"context"
	"encoding/base64"
	"html"
	"io"
	"log"
	"os"
	"regexp"

	"sync"

	"server/llmpool"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

var (
	browser *rod.Browser
	lock    sync.Mutex
)

func initBrowser() {
	path := "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe" // <- change if needed
	u := launcher.New().
		Bin(path).
		Leakless(false).
		Headless(true).
		NoSandbox(true).
		Set("disable-gpu").
		Set("disable-software-rasterizer").
		Set("disable-dev-shm-usage").
		MustLaunch()
	browser = rod.New().ControlURL(u).MustConnect()
}

func extractMetadata(url string) (fiber.Map, error) {
	lock.Lock()
	defer lock.Unlock()

	page := browser.MustPage(url)
	defer page.MustClose()

	page.MustWaitLoad()

	title := page.MustEval(`() => document.title`).String()
	favicon := page.MustEval(`() => {
		const l = document.querySelector("link[rel*='icon']");
		return l ? l.href : "";
	}`).String()

	return fiber.Map{"title": title, "favicon": favicon, "address": url}, nil
}

func extractMetadataFromHTML(html string) (fiber.Map, error) {
	lock.Lock()
	defer lock.Unlock()

	page := browser.MustPage("")
	defer page.MustClose()

	// URL encode the HTML to handle special characters
	encodedHTML := base64.StdEncoding.EncodeToString([]byte(html))
	page.MustNavigate("data:text/html;base64," + encodedHTML)
	page.MustWaitLoad()
	title := page.MustEval(`() => document.title`).String()
	favicon := page.MustEval(`() => {
		const l = document.querySelector("link[rel*='icon']");
		return l ? l.href : "";
	}`).String()

	return fiber.Map{"title": title, "favicon": favicon, "source": "html_content"}, nil
}

func generatePDF(url string) ([]byte, error) {
	lock.Lock()
	defer lock.Unlock()

	page := browser.MustPage(url)
	defer page.MustClose()

	page.MustWaitLoad()

	reader, err := page.PDF(&proto.PagePrintToPDF{
		PrintBackground: true,
	})
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

func generatePDFFromHTML(html string) ([]byte, error) {
	lock.Lock()
	defer lock.Unlock()

	page := browser.MustPage("")
	defer page.MustClose()

	// URL encode the HTML to handle special characters
	encodedHTML := base64.StdEncoding.EncodeToString([]byte(html))
	page.MustNavigate("data:text/html;base64," + encodedHTML)
	page.MustWaitLoad()
	reader, err := page.PDF(&proto.PagePrintToPDF{
		PrintBackground: true,
	})
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}
func checkAuth(res *fiber.Ctx) error {
	if res.Get("Authorization") != "Bearer 123" {
		return res.Status(401).JSON(fiber.Map{"error": "Unauthorized"})
	}
	return res.Next()
}

const systemPrompt string = `
> **If an image is provided as base64, first decode it visually and use it as the design reference for the HTML template.**

You are a **professional invoice template designer** who creates clean, elegant **HTML templates** with **inline CSS** for styling.

You can interpret **base64-encoded images** and recreate their design as **fully self-contained HTML**.

**Variable Naming Rules:**

* Use **double curly braces** for placeholders: {{...}}.
* Use **meaningful names**: {{name}}, {{company_name}}.
* For lists (e.g., invoice items), use **dot notation**: {{list.item_name}}, {{list.price}}, {{list.gst}}.
* **Important:** When showing list/table data, **write only one table row** with placeholders. This will be looped later by the templating engine.

**Design Guidelines:**

1. Output **only HTML with inline CSS**, no external styles or scripts.
2. Keep designs **professional, modern, and readable**.
3. Use semantic HTML structure (e.g., <header>, <table>, <footer>).
4. Ensure consistent spacing, alignment, and typography.
5. If the design contains repeating elements (invoice items, products, etc.), **only include a single placeholder row** in the HTML.
6. Recreate colors, spacing, fonts, and layout from the given base64 image as closely as possible.

**Example (Structure Only):**


<html>
  <body>
    <header>
      <h1>{{company_name}}</h1>
      <p>{{company_address}}</p>
    </header>
    <table>
      <tr>
        <th>Item</th>
        <th>Qty</th>
        <th>Price</th>
      </tr>
      <tr>
        <td>{{list.item_name}}</td>
        <td>{{list.quantity}}</td>
        <td>{{list.price}}</td>
      </tr>
    </table>
    <footer>
      <p>Total: {{total_amount}}</p>
    </footer>
  </body>
</html>


`

type AIResponse struct {
	Response string `json:"response"`
}

func cleanAIHTML(aiResp string) string {
	// Step 1: Strip ```html and ``` markers
	re := regexp.MustCompile("(?s)```html\\s*(.*?)\\s*```")
	matches := re.FindStringSubmatch(aiResp)
	var cleaned string
	if len(matches) > 1 {
		cleaned = matches[1]
	} else {
		cleaned = aiResp
	}

	// Step 2: Unescape \u003c, \u003e, etc.
	return html.UnescapeString(cleaned)
}

func main() {
	initBrowser()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	pool := llmpool.NewPool()
	pool.AddProvider(&llmpool.Provider{
		Name:              "groq-fast",
		Type:              llmpool.ProviderGroq,
		APIKey:            os.Getenv("API_1"),
		BaseURL:           "https://api.groq.com/openai/v1",
		Model:             "openai/gpt-oss-20b",
		Priority:          1,
		RequestsPerMinute: 30,
	})

	app := fiber.New()

	app.Use(func(res *fiber.Ctx) error {
		res.Set("Access-Control-Allow-Origin", "*")
		res.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		res.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

		if res.Method() == "OPTIONS" {
			return res.SendStatus(200)
		}

		return res.Next()
	})
	app.Post("/create/ai", checkAuth, func(res *fiber.Ctx) error {
		var body struct {
			Message string `json:"prompt"`
		}

		if err := res.BodyParser(&body); err != nil {
			return res.Status(400).JSON(fiber.Map{"error": "Invalid JSON body"})
		}
		// Use the pool
		req := &llmpool.ChatRequest{
			Messages: []llmpool.ChatMessage{
				{Role: "system",
					Content: systemPrompt},
				{Role: "user", Content: body.Message},
			},
			Temperature: 0.7,
			MaxTokens:   8000,
		}

		resp, err := pool.Chat(context.Background(), req)
		if err != nil {
			log.Fatal(err)
		}
		//fmt.Print(resp.Content)

		return res.Status(200).JSON(fiber.Map{"response": cleanAIHTML(resp.Content)})

	})
	app.Get("/", func(res *fiber.Ctx) error {
		return res.SendFile("../index.html")
	})
	// Extract metadata from URL
	app.Get("/extract", func(res *fiber.Ctx) error {
		u := res.Query("url")
		if u == "" {
			return res.Status(400).JSON(fiber.Map{"error": "Missing ?url param"})
		}

		meta, err := extractMetadata(u)
		if err != nil {
			return res.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		return res.JSON(meta)
	})

	// Extract metadata from HTML content
	app.Post("/extract-html", func(res *fiber.Ctx) error {
		var body struct {
			HTML string `json:"html"`
		}

		if err := res.BodyParser(&body); err != nil {
			return res.Status(400).JSON(fiber.Map{"error": "Invalid JSON body"})
		}

		if body.HTML == "" {
			return res.Status(400).JSON(fiber.Map{"error": "Missing html field in request body"})
		}

		meta, err := extractMetadataFromHTML(body.HTML)
		if err != nil {
			return res.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		return res.JSON(meta)
	})

	// Generate PDF from URL
	app.Get("/pdf", func(res *fiber.Ctx) error {
		u := res.Query("url")
		if u == "" {
			return res.Status(400).JSON(fiber.Map{"error": "Missing ?url param"})
		}

		pdf, err := generatePDF(u)
		if err != nil {
			return res.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		res.Response().Header.Set("Content-Type", "application/pdf")
		res.Response().Header.Set("Content-Disposition", "inline; filename=result.pdf")
		return res.Send(pdf)
	})

	// Generate PDF from HTML content
	app.Post("/pdf-html", func(res *fiber.Ctx) error {
		var body struct {
			HTML     string `json:"html"`
			Filename string `json:"filename,omitempty"`
		}

		if err := res.BodyParser(&body); err != nil {
			return res.Status(400).JSON(fiber.Map{"error": "Invalid JSON body"})
		}

		if body.HTML == "" {
			return res.Status(400).JSON(fiber.Map{"error": "Missing html field in request body"})
		}

		pdf, err := generatePDFFromHTML(body.HTML)
		if err != nil {
			return res.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		filename := "result.pdf"
		if body.Filename != "" {
			filename = body.Filename
		}

		res.Response().Header.Set("Content-Type", "application/pdf")
		res.Response().Header.Set("Content-Disposition", "inline; filename="+filename)
		return res.Send(pdf)
	})

	// Unified PDF endpoint that supports both URL and HTML
	app.Post("/pdf-unified", func(res *fiber.Ctx) error {
		var body struct {
			URL      string `json:"url,omitempty"`
			HTML     string `json:"html,omitempty"`
			Filename string `json:"filename,omitempty"`
		}

		if err := res.BodyParser(&body); err != nil {
			return res.Status(400).JSON(fiber.Map{"error": "Invalid JSON body"})
		}

		if body.URL == "" && body.HTML == "" {
			return res.Status(400).JSON(fiber.Map{"error": "Either url or html field is required"})
		}

		if body.URL != "" && body.HTML != "" {
			return res.Status(400).JSON(fiber.Map{"error": "Provide either url or html, not both"})
		}

		var pdf []byte
		var err error

		if body.URL != "" {
			pdf, err = generatePDF(body.URL)
		} else {
			pdf, err = generatePDFFromHTML(body.HTML)
		}

		if err != nil {
			return res.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		filename := "result.pdf"
		if body.Filename != "" {
			filename = body.Filename
		}

		res.Response().Header.Set("Content-Type", "application/pdf")
		res.Response().Header.Set("Content-Disposition", "inline; filename="+filename)
		return res.Send(pdf)
	})

	log.Println("Running at http://localhost:8080")
	log.Println("Endpoints:")
	log.Println("  Get /                - get index file")
	log.Println("  POST /create/ai      - generate template via ai pool")
	log.Println("  GET  /extract        - Extract metadata from URL")
	log.Println("  POST /extract-html   - Extract metadata from HTML content")
	log.Println("  GET  /pdf            - Generate PDF from URL")
	log.Println("  POST /pdf-html       - Generate PDF from HTML content")
	log.Println("  POST /pdf-unified    - Generate PDF from either URL or HTML")

	log.Fatal(app.Listen(":8080"))
}
