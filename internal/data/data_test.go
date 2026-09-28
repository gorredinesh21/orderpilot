package data

import (
	"strings"
	"testing"
)

func TestCorpusDeterministic(t *testing.T) {
	a := Get()
	first := a.Items[0].ID + a.Items[0].Name
	total := len(a.Items)
	if total < 400 {
		t.Fatalf("expected a rich corpus, got %d items", total)
	}
	// second access must be the same instance (sync.Once)
	b := Get()
	if a != b {
		t.Fatal("Get should return the same instance")
	}
	_ = first
}

func TestSearchRestaurantsFilters(t *testing.T) {
	c := Get()
	tests := []struct {
		name              string
		cuisine, area     string
		maxPrice, minRat  int
		vegOnly           bool
		wantAtLeast       int
	}{
		{"biryani anywhere", "biryani", "", 0, 0, false, 3},
		{"andhra veg", "andhra", "", 0, 0, true, 0}, // most andhra places are non-veg; must not panic
		{"cheap in HSR", "", "HSR Layout", 400, 0, false, 1},
		{"high rated", "", "", 0, 46, false, 0}, // minRating 46 = 4.6 as int scale trap: 0 expected
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.SearchRestaurants(tt.cuisine, tt.area, tt.maxPrice, tt.minRat, tt.vegOnly)
			if len(got) < tt.wantAtLeast {
				t.Fatalf("got %d restaurants, want ≥ %d", len(got), tt.wantAtLeast)
			}
			for _, r := range got {
				if tt.area != "" && !strings.EqualFold(r.Area, tt.area) && !strings.Contains(strings.ToLower(r.Area), strings.ToLower(tt.area)) {
					t.Errorf("restaurant %s in wrong area %s", r.Name, r.Area)
				}
				if tt.maxPrice > 0 && r.PriceForTwo > tt.maxPrice {
					t.Errorf("restaurant %s over price filter", r.Name)
				}
				if tt.vegOnly && !r.VegOnly {
					t.Errorf("non-veg restaurant %s in veg-only results", r.Name)
				}
			}
		})
	}
}

func TestSearchRestaurantsRanking(t *testing.T) {
	rs := Get().SearchRestaurants("biryani", "", 0, 0, false)
	if len(rs) < 2 {
		t.Skip("not enough results")
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].Rating > rs[i-1].Rating {
			t.Fatalf("results not sorted by rating desc: %v then %v", rs[i-1].Rating, rs[i].Rating)
		}
	}
}

func TestSearchDishes(t *testing.T) {
	c := Get()
	items := c.SearchDishes("biryani", 0, false, 0)
	if len(items) == 0 {
		t.Fatal("expected biryani dishes")
	}
	// every hit must mention the term somewhere the search looks — including
	// the restaurant name (a biryani place's drinks legitimately match)
	for _, it := range items {
		hay := strings.ToLower(it.Name + " " + it.Description + " " + it.Category + " " + it.RestName)
		if !strings.Contains(hay, "biryani") {
			t.Errorf("dish %s does not mention biryani", it.Name)
		}
	}
	// price filter respected
	for _, it := range c.SearchDishes("dosa", 100, false, 0) {
		if it.Price > 100 {
			t.Errorf("dish %s exceeds max price", it.Name)
		}
	}
	// veg filter respected
	for _, it := range c.SearchDishes("chicken", 0, true, 0) {
		if !it.Veg {
			t.Errorf("non-veg dish %s in veg-only results", it.Name)
		}
	}
}

func TestHaversine(t *testing.T) {
	// Koramangala -> Indiranagar is roughly 5-6 km
	d := HaversineKm(12.9345, 77.6266, 12.9784, 77.6408)
	if d < 3 || d > 8 {
		t.Fatalf("Koramangala→Indiranagaar distance %.1f km outside plausible band", d)
	}
	if HaversineKm(12.9345, 77.6266, 12.9345, 77.6266) != 0 {
		t.Fatal("same point must be 0 km")
	}
}

func TestEveryRestaurantHasItemsAndDrinks(t *testing.T) {
	c := Get()
	for _, r := range c.Restaurants {
		menu := c.Menu(r.ID)
		if len(menu) < 8 {
			t.Errorf("restaurant %s has only %d items", r.Name, len(menu))
		}
		hasDrink := false
		for _, it := range menu {
			if it.Category == "Beverage" {
				hasDrink = true
				break
			}
		}
		if !hasDrink {
			t.Errorf("restaurant %s has no beverage", r.Name)
		}
		// veg-only restaurants must not serve meat
		if r.VegOnly {
			for _, it := range menu {
				if !it.Veg {
					t.Errorf("veg-only restaurant %s serves %s", r.Name, it.Name)
				}
			}
		}
	}
}

func TestItemLookup(t *testing.T) {
	c := Get()
	if _, ok := c.ItemByID("nope"); ok {
		t.Fatal("bogus id must not resolve")
	}
	it, ok := c.ItemByID(c.Items[0].ID)
	if !ok || it.ID != c.Items[0].ID {
		t.Fatal("real id must resolve to itself")
	}
}
