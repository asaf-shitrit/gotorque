package campaign

import "math"

// nullConfidence is the one-sided level the null-candidate rates are bounded
// at. A campaign of nulls states a claim of the shape "0 accepts in 70 runs",
// and the bound is what makes that claim mean something: the true false-accept
// rate is below the printed figure with this probability.
const nullConfidence = 0.95

// clopperPearsonUpper returns the exact one-sided 95% Clopper-Pearson upper
// bound on a rate observed as hits in runs. It is the p at which the chance of
// seeing at most hits hits is 1 - nullConfidence, which is the
// nullConfidence quantile of the Beta(hits+1, runs-hits) distribution. For no
// hits in n runs that reduces to 1 - 0.05^(1/n), about 3/n.
//
// Exact rather than normal-approximate on purpose: the useful reading is
// usually zero hits, where a Wald interval collapses to a zero-width interval
// at zero and says nothing.
func clopperPearsonUpper(hits, runs int) float64 {
	switch {
	case runs <= 0 || hits >= runs:
		return 1
	case hits <= 0:
		return -math.Expm1(math.Log(1-nullConfidence) / float64(runs))
	}
	return betaQuantile(nullConfidence, float64(hits+1), float64(runs-hits))
}

// betaQuantile is the p-quantile of the Beta(a, b) distribution, found by
// bisection on the regularized incomplete beta function. The function is
// monotone in x and the interval starts at [0, 1], so 100 halvings reach the
// precision of a float64; there is no need for a starting guess.
func betaQuantile(p, a, b float64) float64 {
	lo, hi := 0.0, 1.0
	for range 100 {
		mid := (lo + hi) / 2
		if regularizedBeta(a, b, mid) < p {
			lo = mid
			continue
		}
		hi = mid
	}
	return (lo + hi) / 2
}

// regularizedBeta is the regularized incomplete beta function I_x(a, b). The
// continued fraction is the one from Numerical Recipes (betacf); it converges
// quickly for the small integer parameters a rate bound uses.
func regularizedBeta(a, b, x float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	logBeta := lgamma(a+b) - lgamma(a) - lgamma(b)
	front := math.Exp(logBeta + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaContinuedFraction(a, b, x) / a
	}
	return 1 - front*betaContinuedFraction(b, a, 1-x)/b
}

// betaContinuedFraction evaluates the continued fraction of the incomplete
// beta function (betacf in Numerical Recipes).
func betaContinuedFraction(a, b, x float64) float64 {
	const (
		maxIterations = 200
		epsilon       = 3e-14
	)
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 / nonZero(1-qab*x/qap)
	h := d
	for m := 1; m <= maxIterations; m++ {
		m2 := 2 * float64(m)
		aa := float64(m) * (b - float64(m)) * x / ((qam + m2) * (a + m2))
		d = 1 / nonZero(1+aa*d)
		c = nonZero(1 + aa/c)
		h *= d * c
		aa = -(a + float64(m)) * (qab + float64(m)) * x / ((a + m2) * (qap + m2))
		d = 1 / nonZero(1+aa*d)
		c = nonZero(1 + aa/c)
		del := d * c
		h *= del
		if math.Abs(del-1) < epsilon {
			break
		}
	}
	return h
}

// lgamma is math.Lgamma's value without the sign, which an incomplete beta
// never needs.
func lgamma(v float64) float64 {
	value, _ := math.Lgamma(v)
	return value
}

// nonZero keeps a continued fraction's denominators off zero.
func nonZero(v float64) float64 {
	const tiny = 1e-300
	if math.Abs(v) < tiny {
		return tiny
	}
	return v
}
