package fixture

// TreeNode is self-referential through a named (non-anonymous, non-registered)
// field, so a walker that recurses into every nested struct type without
// cycle detection would never terminate. Recursion into Child must stop the
// moment TreeNode reappears on the current path, keeping Child itself as a
// terminal field rather than expanding forever.
type TreeNode struct {
	Value string    `json:"value"`
	Child *TreeNode `json:"child,omitempty"`
}
