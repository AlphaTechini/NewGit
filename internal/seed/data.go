package seed

import "github.com/AlphaTechini/newgit/internal/store"

type commit struct {
	message string
	files   []store.FileChange
}

type issue struct {
	title, body string
	comments    []string
	closed      bool
}

type mergeRequest struct {
	title, description string
	files              []store.FileChange
	comments           []string
}

var history = []commit{
	{"Start the storefront API", []store.FileChange{
		{Path: "README.md", Content: readme},
		{Path: "go.mod", Content: "module example.com/storefront\n\ngo 1.22\n"},
		{Path: "main.go", Content: mainGo},
	}},
	{"Add product handlers", []store.FileChange{
		{Path: "products.go", Content: productsGo},
	}},
	{"Add the cart", []store.FileChange{
		{Path: "cart.go", Content: cartGo},
	}},
	{"Document the API", []store.FileChange{
		{Path: "docs/api.md", Content: apiMd},
	}},
	{"Validate quantities in the cart", []store.FileChange{
		{Path: "cart.go", Content: cartGo + cartValidate},
	}},
	{"Add a changelog", []store.FileChange{
		{Path: "CHANGELOG.md", Content: "# Changelog\n\n## 0.2.0\n\n- Cart quantities are validated.\n\n## 0.1.0\n\n- Products and cart endpoints.\n"},
	}},
}

var issues = []issue{
	{"Cart total ignores discounts", "When a product has a discount, `GET /cart` still shows the full price.\n\nSteps:\n1. Add a discounted product\n2. Open the cart\n\nExpected the discounted total.", []string{"Confirmed on main.", "The total is summed before discounts apply in `cart.go`."}, false},
	{"Add pagination to the product list", "`GET /products` returns everything. With a large catalogue this gets slow. Add `?page=` and `?per_page=`.", []string{"A cursor would avoid skipped rows when products are added mid-scroll."}, false},
	{"Document error responses", "`docs/api.md` lists the happy paths only. Add the 400 and 404 bodies.", nil, false},
	{"Typo in README", "\"storefornt\" in the first paragraph.", []string{"Fixed in the README rewrite."}, true},
	{"Support product images", "Products need an image URL so the shop front can show a thumbnail.", []string{"Store a URL only, no uploads for now."}, false},
	{"Return 404 for unknown products", "`GET /products/999` returns 200 with an empty body.", nil, false},
}

var mergeRequests = []mergeRequest{
	{"Apply discounts before summing the cart", "Fixes the cart total for discounted products.", []store.FileChange{
		{Path: "cart.go", Content: cartGo + cartValidate + cartDiscount},
	}, []string{"Looks right. Can we add a test for a 100% discount?"}},
	{"Paginate the product list", "Adds `page` and `per_page` to `GET /products`, defaulting to 20 per page.", []store.FileChange{
		{Path: "products.go", Content: productsGo + productsPaging},
	}, nil},
}

const readme = `# storefront

A small shop API: products, a cart and checkout.

## Run

` + "```sh\ngo run .\n```" + `

The server listens on :8080.

## Endpoints

| Method | Path | What it does |
|---|---|---|
| GET | /products | List products |
| GET | /products/{id} | One product |
| POST | /cart | Add an item |
| GET | /cart | Show the cart and its total |

See [docs/api.md](docs/api.md) for request and response bodies.
`

const mainGo = `package main

import (
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /products", listProducts)
	mux.HandleFunc("GET /products/{id}", getProduct)
	mux.HandleFunc("POST /cart", addToCart)
	mux.HandleFunc("GET /cart", showCart)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
`

const productsGo = `package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

type Product struct {
	ID       int     ` + "`json:\"id\"`" + `
	Name     string  ` + "`json:\"name\"`" + `
	Price    float64 ` + "`json:\"price\"`" + `
	Discount float64 ` + "`json:\"discount\"`" + `
}

var products = []Product{
	{1, "Notebook", 4.50, 0},
	{2, "Pen", 1.20, 0.1},
	{3, "Backpack", 39.00, 0.25},
}

func listProducts(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(products)
}

func getProduct(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	for _, p := range products {
		if p.ID == id {
			json.NewEncoder(w).Encode(p)
			return
		}
	}
}
`

const productsPaging = `
func page(r *http.Request) (start, end int) {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 20
	}
	start = n * per
	return start, start + per
}
`

const cartGo = `package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

type item struct {
	ProductID int ` + "`json:\"product_id\"`" + `
	Quantity  int ` + "`json:\"quantity\"`" + `
}

var (
	mu   sync.Mutex
	cart []item
)

func addToCart(w http.ResponseWriter, r *http.Request) {
	var it item
	if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	mu.Lock()
	cart = append(cart, it)
	mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func showCart(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	total := 0.0
	for _, it := range cart {
		for _, p := range products {
			if p.ID == it.ProductID {
				total += p.Price * float64(it.Quantity)
			}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"items": cart, "total": total})
}
`

const cartValidate = `
func validQuantity(q int) bool { return q > 0 && q <= 99 }
`

const cartDiscount = `
func discounted(p Product) float64 { return p.Price * (1 - p.Discount) }
`

const apiMd = `# API

## GET /products

Returns every product.

` + "```json\n[{\"id\": 1, \"name\": \"Notebook\", \"price\": 4.5, \"discount\": 0}]\n```" + `

## POST /cart

` + "```json\n{\"product_id\": 1, \"quantity\": 2}\n```" + `

Returns 201 with no body.

## GET /cart

` + "```json\n{\"items\": [{\"product_id\": 1, \"quantity\": 2}], \"total\": 9}\n```" + `
`
