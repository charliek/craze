The problem was introduced in c856921 "Support per-customer tax rates". That commit changed `total()` to apply tax after the discount, and the extra multiplication introduces floating-point error, so some totals come out a cent low.

Fix: apply the tax to the subtotal before the discount, or round twice.
