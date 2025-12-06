// Package workload generates cache workload patterns.
package workload

import (
	"math"
	"math/rand/v2"
	"strconv"
)

// GenerateZipf generates a Zipfian distribution of keys as strings ("key-X").
func GenerateZipf(n, keySpace int, theta float64, seed uint64) []string {
	ints := GenerateZipfInt(n, keySpace, theta, seed)
	keys := make([]string, n)
	for i, v := range ints {
		keys[i] = "key-" + strconv.Itoa(v)
	}
	return keys
}

// GenerateZipfInt generates a Zipfian distribution of keys as integers.
func GenerateZipfInt(n, keySpace int, theta float64, seed uint64) []int {
	rng := rand.New(rand.NewPCG(seed, seed+1)) //nolint:gosec // G404 is a false positive for math/rand/v2, this is not for cryptographic use.
	keys := make([]int, n)

	spread := keySpace + 1
	zeta2 := computeZeta(2, theta)
	zetaN := computeZeta(uint64(spread), theta) //nolint:gosec // G115: spread is always non-negative as keySpace >= 0.
	alpha := 1.0 / (1.0 - theta)
	eta := (1 - math.Pow(2.0/float64(spread), 1.0-theta)) / (1.0 - zeta2/zetaN)
	halfPowTheta := 1.0 + math.Pow(0.5, theta)

	for i := range n {
		u := rng.Float64()
		uz := u * zetaN
		var result int
		switch {
		case uz < 1.0:
			result = 0
		case uz < halfPowTheta:
			result = 1
		default:
			result = int(float64(spread) * math.Pow(eta*u-eta+1.0, alpha))
		}
		if result >= keySpace {
			result = keySpace - 1
		}
		keys[i] = result
	}
	return keys
}

// computeZeta calculates zeta(n, theta) = sum(1/i^theta) for i=1 to n.
func computeZeta(n uint64, theta float64) float64 {
	sum := 0.0
	for i := uint64(1); i <= n; i++ {
		sum += 1.0 / math.Pow(float64(i), theta)
	}
	return sum
}
