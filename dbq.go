// Copyright 2019 PJ Engineering and Business Solutions Pty. Ltd. All rights reserved.

package dbq

import (
	"context"
	stdSql "database/sql"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/mitchellh/mapstructure"
)

// StructorConfig is used to expose a subset of the configuration options
// provided by the mapstructure package.
//
// See: https://godoc.org/github.com/mitchellh/mapstructure#DecoderConfig
type StructorConfig struct {

	// DecodeHook, if set, will be called before any decoding and any
	// type conversion (if WeaklyTypedInput is on). This lets you modify
	// the values before they're set down onto the resulting struct.
	//
	// If an error is returned, the entire decode will fail with that
	// error.
	DecodeHook mapstructure.DecodeHookFunc

	// If WeaklyTypedInput is true, the decoder will make the following
	// "weak" conversions:
	//
	//   - bools to string (true = "1", false = "0")
	//   - numbers to string (base 10)
	//   - bools to int/uint (true = 1, false = 0)
	//   - strings to int/uint (base implied by prefix)
	//   - int to bool (true if value != 0)
	//   - string to bool (accepts: 1, t, T, TRUE, true, True, 0, f, F,
	//     FALSE, false, False. Anything else is an error)
	//   - empty array = empty map and vice versa
	//   - negative numbers to overflowed uint values (base 10)
	//   - slice of maps to a merged map
	//   - single values are converted to slices if required. Each
	//     element is weakly decoded. For example: "4" can become []int{4}
	//     if the target type is an int slice.
	//
	WeaklyTypedInput bool
}

// SingleResult is a convenience option for the common case of expecting
// a single result from a query.
var SingleResult = &Options{SingleResult: true}

// Panic is a convenience option for the common case of panicing
// upon encountering an error.
var Panic = &Options{Panic: true}

type Options struct {

	// ConcreteStruct can be set to any concrete struct (not a pointer).
	// When set, the mapstructure package is used to convert the returned
	// results automatically from a map to a struct. The `dbq` struct tag
	// can be used to map column names to the struct's fields.
	//
	// See: https://godoc.org/github.com/mitchellh/mapstructure
	ConcreteStruct interface{}

	// DecoderConfig is used to configure the decoder used by the mapstructure
	// package.
	//
	// See: https://godoc.org/github.com/mitchellh/mapstructure
	DecoderConfig *StructorConfig

	// SingleResult can be set to true if you know the query will return at most 1 result.
	// When true, a nil is returned if no result is found. Alternatively, it will return the
	// single result directly (instead of wrapped in a slice). This makes it easier to
	// type assert.
	SingleResult bool

	// Panic is used to generate a panic instead of return an error.
	// This can erradicate boiler-plate error handing code.
	Panic bool
}

// E is a wrapper around the Q function. It is used for "Exec" queries such as insert, update and delete.
// It also returns a sql.Result interface instead of a empty interface.
func E(ctx context.Context, pool SQLBasic, query string, options *Options, args ...interface{}) (stdSql.Result, error) {

	query2 := strings.ToLower(strings.TrimSpace(query))

	if !(strings.HasPrefix(query2, "insert") || strings.HasPrefix(query2, "update") ||
		strings.HasPrefix(query2, "delete")) {
		panic("incorrect query type")
	}

	res, err := Q(ctx, pool, query, options, args...)
	if err != nil {
		return nil, err
	}

	return res.(stdSql.Result), nil
}

// Q is a convenience function that is used for inserting, updating, deleting and querying a SQL database.
// For inserts, updates and deletes, a sql.Result is returned.
// For queries, a []map[string]interface{} is ordinarily returned. Each result (item in slice) contains
// a map where the keys are the table columns and the values are the data.
// When a ConcreteStruct is provided via the Options, the mapstructure package is used to automatically
// return []structs instead.
//
// NOTE: sql.ErrNoRows is never returned as an error. Usually a single item slice is returned, unless the
// behavior is modified by the SingleResult Option.
func Q(ctx context.Context, pool SQLBasic, query string, options *Options, args ...interface{}) (out interface{}, rErr error) {

	var (
		o        Options
		wasQuery bool
	)

	if options != nil {
		o = *options
	}

	defer func() {
		if rErr != nil && o.Panic {
			panic(rErr)
		}
		if rErr == nil && wasQuery && o.SingleResult {
			rows := reflect.ValueOf(out)
			if rows.Len() == 0 {
				out = nil
			} else {
				row := rows.Index(0)
				out = row.Interface()
			}
		}
	}()

	query = strings.TrimSpace(query)

	if len(args) == 1 {
		if arg := reflect.ValueOf(args[0]); arg.Kind() == reflect.Slice {
			newArgs := []interface{}{}
			for i := 0; i < arg.Len(); i++ {
				newArgs = append(newArgs, arg.Index(i).Interface())
			}
			args = newArgs
		}
	}

	if strings.HasPrefix(query, "INSERT") || strings.HasPrefix(query, "insert") {
		return pool.ExecContext(ctx, query, args...)
	} else if strings.HasPrefix(query, "UPDATE") || strings.HasPrefix(query, "update") {
		return pool.ExecContext(ctx, query, args...)
	} else if strings.HasPrefix(query, "DELETE") || strings.HasPrefix(query, "delete") {
		return pool.ExecContext(ctx, query, args...)
	} else {
		wasQuery = true // Assume Query

		out := []interface{}{}

		rows, err := pool.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		cols, err := rows.ColumnTypes()
		totalColumns := len(cols)

		// Load decoder
		var decoder *mapstructure.Decoder
		if o.ConcreteStruct != nil {
			res := reflect.New(reflect.TypeOf(o.ConcreteStruct)).Interface()
			if o.DecoderConfig != nil {

				dc := &mapstructure.DecoderConfig{
					DecodeHook:       o.DecoderConfig.DecodeHook,
					ZeroFields:       true,
					TagName:          "dbq",
					WeaklyTypedInput: o.DecoderConfig.WeaklyTypedInput,
					Result:           res,
				}

				decoder, err = mapstructure.NewDecoder(dc)
				if err != nil {
					panic(err)
				}
			}
		}

		for rows.Next() {

			rowData := make([]interface{}, totalColumns)
			for i := range rowData {
				rowData[i] = &[]byte{}
			}

			if err := rows.Scan(rowData...); err != nil {
				return nil, err
			}

			vals := map[string]interface{}{}
			for colID, elem := range rowData {

				colType := cols[colID].DatabaseTypeName()
				fieldName := cols[colID].Name()
				nullable, _ := cols[colID].Nullable()

				var val *string

				raw := elem.(*[]byte)
				if !(raw == nil || *raw == nil) {
					val = &[]string{string(*raw)}[0]
				}

				switch colType {
				case "NULL":
					vals[fieldName] = nil
				case "CHAR", "VARCHAR", "TEXT", "NVARCHAR", "MEDIUMTEXT", "LONGTEXT":
					if nullable {
						vals[fieldName] = val
					} else {
						vals[fieldName] = *val
					}
				case "FLOAT", "DOUBLE", "DECIMAL", "NUMERIC", "FLOAT4", "FLOAT8":
					if nullable {
						if val == nil {
							vals[fieldName] = (*float64)(nil)
						} else {
							f, _ := strconv.ParseFloat(*val, 64)
							vals[fieldName] = &f
						}
					} else {
						f, _ := strconv.ParseFloat(*val, 64)
						vals[fieldName] = f
					}
				case "INT", "TINYINT", "INT2", "INT4", "INT8", "MEDIUMINT", "SMALLINT", "BIGINT":

					var (
						i64 *int64
						u64 *uint64
					)

					if val != nil {
						if n, err := strconv.ParseInt(*val, 10, 64); err == nil {
							i64 = &n
						}
						if u, err := strconv.ParseUint(*val, 10, 64); err == nil {
							u64 = &u
						}
					}

					switch cols[colID].ScanType().Kind() {
					case reflect.Uint:
						if nullable {
							if val == nil {
								vals[fieldName] = (*uint)(nil)
							} else {
								vals[fieldName] = &[]uint{uint(*u64)}[0]
							}
						} else {
							vals[fieldName] = uint(*u64)
						}
					case reflect.Uint8:
						if nullable {
							if val == nil {
								vals[fieldName] = (*uint8)(nil)
							} else {
								vals[fieldName] = &[]uint8{uint8(*u64)}[0]
							}
						} else {
							vals[fieldName] = uint8(*u64)
						}
					case reflect.Uint16:
						if nullable {
							if val == nil {
								vals[fieldName] = (*uint16)(nil)
							} else {
								vals[fieldName] = &[]uint16{uint16(*u64)}[0]
							}
						} else {
							vals[fieldName] = uint16(*u64)
						}
					case reflect.Uint32:
						if nullable {
							if val == nil {
								vals[fieldName] = (*uint32)(nil)
							} else {
								vals[fieldName] = &[]uint32{uint32(*u64)}[0]
							}
						} else {
							vals[fieldName] = uint32(*u64)
						}
					case reflect.Uint64:
						if nullable {
							if val == nil {
								vals[fieldName] = (*uint64)(nil)
							} else {
								vals[fieldName] = &[]uint64{*u64}[0]
							}
						} else {
							vals[fieldName] = *u64
						}
					case reflect.Int:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int)(nil)
							} else {
								vals[fieldName] = &[]int{int(*i64)}[0]
							}
						} else {
							vals[fieldName] = int(*i64)
						}
					case reflect.Int8:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int8)(nil)
							} else {
								vals[fieldName] = &[]int8{int8(*i64)}[0]
							}
						} else {
							vals[fieldName] = int8(*i64)
						}
					case reflect.Int16:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int16)(nil)
							} else {
								vals[fieldName] = &[]int16{int16(*i64)}[0]
							}
						} else {
							vals[fieldName] = int16(*i64)
						}
					case reflect.Int32:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int32)(nil)
							} else {
								vals[fieldName] = &[]int32{int32(*i64)}[0]
							}
						} else {
							vals[fieldName] = int32(*i64)
						}
					case reflect.Int64:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int64)(nil)
							} else {
								vals[fieldName] = &[]int64{*i64}[0]
							}
						} else {
							vals[fieldName] = *i64
						}
					default:
						if nullable {
							if val == nil {
								vals[fieldName] = (*int64)(nil)
							} else {
								vals[fieldName] = &[]int64{*i64}[0]
							}
						} else {
							vals[fieldName] = *i64
						}
					}
				case "BOOL":
					if nullable {
						if val == nil {
							vals[fieldName] = (*bool)(nil)
						} else {
							if *val == "true" || *val == "TRUE" || *val == "1" {
								vals[fieldName] = &[]bool{true}[0]
							} else {
								vals[fieldName] = &[]bool{false}[0]
							}
						}
					} else {
						if *val == "true" || *val == "TRUE" || *val == "1" {
							vals[fieldName] = true
						} else {
							vals[fieldName] = false
						}
					}
				case "DATETIME", "TIMESTAMP", "TIMESTAMPTZ":
					if nullable {
						if val == nil {
							vals[fieldName] = (*time.Time)(nil)
						} else {
							t, _ := time.Parse(time.RFC3339, *val)
							vals[fieldName] = &t
						}
					} else {
						t, _ := time.Parse(time.RFC3339, *val)
						vals[fieldName] = t
					}
				case "JSON", "JSONB":
					if nullable && val == nil {
						vals[fieldName] = nil
					} else {
						var jData interface{}
						json.Unmarshal(*raw, &jData)
						vals[fieldName] = jData
					}
				case "DATE":
					if nullable {
						if val == nil {
							vals[fieldName] = (*civil.Date)(nil)
						} else {
							d, _ := civil.ParseDate(*val)
							vals[fieldName] = &d
						}
					} else {
						d, _ := civil.ParseDate(*val)
						vals[fieldName] = d
					}
				case "TIME":
					if nullable {
						if val == nil {
							vals[fieldName] = (*civil.Time)(nil)
						} else {
							t, _ := civil.ParseTime(*val)
							vals[fieldName] = &t
						}
					} else {
						t, _ := civil.ParseTime(*val)
						vals[fieldName] = t
					}

				// TODO: More data types
				// https://github.com/go-sql-driver/mysql/blob/master/fields.go
				// https://github.com/lib/pq/blob/master/oid/types.go
				default:
					// Assume string
					if nullable {
						vals[fieldName] = val
					} else {
						if val == nil {
							vals[fieldName] = (*string)(nil)
						} else {
							vals[fieldName] = *val
						}
					}
				}
			}

			if o.ConcreteStruct != nil {
				res := reflect.New(reflect.TypeOf(o.ConcreteStruct)).Interface()
				if o.DecoderConfig != nil {
					err = decoder.Decode(vals)
					if err != nil {
						return nil, err
					}
				} else {
					err := mapstructure.Decode(vals, &res)
					if err != nil {
						return nil, err
					}
				}
				out = append(out, res)
			} else {
				out = append(out, vals)
			}

		}

		err = rows.Close()
		if err != nil {
			return nil, err
		}

		if err := rows.Err(); err != nil {
			return nil, err
		}

		return out, nil
	}

	return nil, nil
}
