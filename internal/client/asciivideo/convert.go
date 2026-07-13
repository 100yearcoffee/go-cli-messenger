package asciivideo

const DefaultCharset = " .,:;irsXA253hMHGS#9B&@"

// FromGray maps a grayscale image to terminal cells. sourceRows should be
// twice rows to compensate for the typical terminal cell aspect ratio.
func FromGray(pixels []byte, sourceColumns, sourceRows, columns, rows int) ([]byte, error) {
	if sourceColumns != columns || sourceRows != rows*2 || len(pixels) != sourceColumns*sourceRows {
		return nil, ErrInvalidFrame
	}
	result := make([]byte, columns*rows)
	for row := 0; row < rows; row++ {
		for column := 0; column < columns; column++ {
			top := int(pixels[(row*2)*sourceColumns+column])
			bottom := int(pixels[(row*2+1)*sourceColumns+column])
			brightness := (top + bottom) / 2
			result[row*columns+column] = DefaultCharset[brightness*(len(DefaultCharset)-1)/255]
		}
	}
	return result, nil
}
