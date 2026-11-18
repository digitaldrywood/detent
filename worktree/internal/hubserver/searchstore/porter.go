package searchstore

import "strings"

func consonant(word string, i int) bool {
	switch word[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		return i == 0 || !consonant(word, i-1)
	default:
		return true
	}
}

func measure(word string) int {
	m, vowel := 0, false
	for i := range len(word) {
		if consonant(word, i) {
			if vowel {
				m++
			}
			vowel = false
		} else {
			vowel = true
		}
	}
	return m
}

func hasVowel(word string) bool {
	for i := range len(word) {
		if !consonant(word, i) {
			return true
		}
	}
	return false
}

func doubleConsonant(word string) bool {
	n := len(word)
	return n >= 2 && word[n-1] == word[n-2] && consonant(word, n-1)
}

func cvc(word string) bool {
	n := len(word)
	if n < 3 || !consonant(word, n-1) || consonant(word, n-2) || !consonant(word, n-3) {
		return false
	}
	return !strings.ContainsRune("wxy", rune(word[n-1]))
}

func stem(word string) string {
	if len(word) < 3 || len(word) > 64 {
		return word
	}
	for i := range len(word) {
		if word[i] < 'a' || word[i] > 'z' {
			return word
		}
	}
	switch {
	case strings.HasSuffix(word, "sses"):
		word = word[:len(word)-2]
	case strings.HasSuffix(word, "ies"):
		word = word[:len(word)-2]
	case strings.HasSuffix(word, "ss"):
	case strings.HasSuffix(word, "s"):
		word = word[:len(word)-1]
	}
	if root, ok := strings.CutSuffix(word, "eed"); ok {
		if measure(root) > 0 {
			word = root + "ee"
		}
	} else {
		root, ok := strings.CutSuffix(word, "ed")
		if !ok {
			root, ok = strings.CutSuffix(word, "ing")
		}
		if ok && hasVowel(root) {
			word = root
			switch {
			case strings.HasSuffix(word, "at"), strings.HasSuffix(word, "bl"), strings.HasSuffix(word, "iz"):
				word += "e"
			case doubleConsonant(word):
				if !strings.ContainsRune("lsz", rune(word[len(word)-1])) {
					word = word[:len(word)-1]
				}
			case measure(word) == 1 && cvc(word):
				word += "e"
			}
		}
	}
	if root, ok := strings.CutSuffix(word, "y"); ok && hasVowel(root) {
		word = root + "i"
	}
	for _, step := range [][][2]string{
		{{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"}, {"izer", "ize"}, {"bli", "ble"}, {"alli", "al"}, {"entli", "ent"}, {"eli", "e"}, {"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"}, {"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"}, {"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"}, {"logi", "log"}},
		{{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""}},
	} {
		for _, rule := range step {
			if root, ok := strings.CutSuffix(word, rule[0]); ok {
				if measure(root) > 0 {
					word = root + rule[1]
				}
				break
			}
		}
	}
	for _, suffix := range []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize"} {
		if root, ok := strings.CutSuffix(word, suffix); ok {
			if measure(root) > 1 && (suffix != "ion" || strings.HasSuffix(root, "s") || strings.HasSuffix(root, "t")) {
				word = root
			}
			break
		}
	}
	if root, ok := strings.CutSuffix(word, "e"); ok {
		m := measure(root)
		if m > 1 || m == 1 && !cvc(root) {
			word = root
		}
	}
	if measure(word) > 1 && doubleConsonant(word) && strings.HasSuffix(word, "l") {
		word = word[:len(word)-1]
	}
	return word
}
